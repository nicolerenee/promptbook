package nfo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
)

// sampleRecordings returns two synthetic recordings for the same show
// with parseable years 2017 and 2024 so earliestYear can pick the
// smaller one.
func sampleRecordings() []encora.Recording {
	return []encora.Recording{
		{
			ID:   101,
			Show: "Halcyon Crossing",
			Date: encora.Date{FullDate: "2024-06-12", MonthKnown: true, DayKnown: true},
		},
		{
			ID:   102,
			Show: "Halcyon Crossing",
			Date: encora.Date{FullDate: "2017-04-10", MonthKnown: true, DayKnown: true},
		},
	}
}

// TestWriteShowCollectionNoPoster verifies the no-poster path produces
// a valid <collection> document with <name>/<sorttitle>/<year> but no
// <thumb>/<fanart> elements.
func TestWriteShowCollectionNoPoster(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	written, err := nfo.WriteShowCollectionFile(t.Context(), nfo.ShowWriteOptions{
		ShowID:     999,
		ShowName:   "Halcyon Crossing",
		Dir:        dir,
		Recordings: sampleRecordings(),
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "collection.nfo"), written)

	got, err := os.ReadFile(written)
	require.NoError(t, err)
	body := string(got)

	assert.Contains(t, body, `<?xml version="1.0"`)
	assert.Contains(t, body, `<collection>`)
	assert.Contains(t, body, `<name>Halcyon Crossing</name>`)
	assert.Contains(t, body, `<sorttitle>Halcyon Crossing</sorttitle>`)
	assert.Contains(t, body, `<year>2017</year>`,
		"earliest year across recordings should win")
	assert.Contains(t, body, `<uniqueid type="encora-show" default="true">999</uniqueid>`)
	assert.NotContains(t, body, `<thumb>`)
	assert.NotContains(t, body, `<fanart>`)
}

// TestWriteShowCollectionWithPoster verifies the poster path: when a
// banner.jpg is cached for the show, the <thumb> + <fanart><thumb>
// elements appear with a forward-slash relative path computed against
// Dir. Selection is implicit by file existence under the v2 layout.
func TestWriteShowCollectionWithPoster(t *testing.T) {
	t.Parallel()

	const showID int64 = 4711
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	writeFakeImage(t, cache.ShowBannerPath(showID))

	dir := t.TempDir()
	written, err := nfo.WriteShowCollectionFile(t.Context(), nfo.ShowWriteOptions{
		Cache:      cache,
		ShowID:     showID,
		ShowName:   "Halcyon Crossing",
		Dir:        dir,
		Recordings: sampleRecordings(),
	})
	require.NoError(t, err)

	got, err := os.ReadFile(written)
	require.NoError(t, err)
	body := string(got)

	expectedRel, relErr := filepath.Rel(dir, cache.ShowBannerPath(showID))
	require.NoError(t, relErr)
	assert.Contains(t, body, `<thumb>`+filepath.ToSlash(expectedRel)+`</thumb>`)
	assert.Contains(t, body, `<fanart>`)
}

// TestWriteShowCollectionWithDescription verifies a non-empty
// Description renders as a <plot> element with HTML stripped.
func TestWriteShowCollectionWithDescription(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := nfo.WriteShowCollectionFile(t.Context(), nfo.ShowWriteOptions{
		ShowID:      1,
		ShowName:    "Halcyon Crossing",
		Description: "<p>An epic retelling.</p>",
		Dir:         dir,
		Recordings:  sampleRecordings(),
	})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "collection.nfo"))
	require.NoError(t, err)
	assert.Contains(t, string(got), `<plot>An epic retelling.</plot>`)
}

// TestWriteShowCollectionMissingDescriptionOmitsPlot asserts that an
// empty Description omits the <plot> element entirely (rather than
// emitting an empty one).
func TestWriteShowCollectionMissingDescriptionOmitsPlot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := nfo.WriteShowCollectionFile(t.Context(), nfo.ShowWriteOptions{
		ShowID:     1,
		ShowName:   "Halcyon Crossing",
		Dir:        dir,
		Recordings: sampleRecordings(),
	})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "collection.nfo"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), `<plot>`)
}

// TestWriteShowCollectionRejectsEmptyDir verifies a missing Dir or
// ShowName fails fast rather than silently writing a malformed file.
func TestWriteShowCollectionRejectsEmptyArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts nfo.ShowWriteOptions
	}{
		{name: "empty_dir", opts: nfo.ShowWriteOptions{ShowID: 1, ShowName: "X"}},
		{name: "empty_name", opts: nfo.ShowWriteOptions{ShowID: 1, Dir: t.TempDir()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := nfo.WriteShowCollectionFile(t.Context(), tt.opts)
			assert.Error(t, err)
		})
	}
}
