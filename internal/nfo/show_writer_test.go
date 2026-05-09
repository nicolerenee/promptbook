package nfo_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/storage"
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
// poster is cached and a show_image_choices row points at it, the
// <thumb> + <fanart><thumb> elements appear with a forward-slash
// relative path computed against Dir.
func TestWriteShowCollectionWithPoster(t *testing.T) {
	t.Parallel()

	const showID int64 = 4711
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	// Stage two posters; the choice row pins index 1 so resolution
	// must honor it (not fall through to the default 0).
	writeFakeImage(t, cache.PosterPath(showID, 0))
	writeFakeImage(t, cache.PosterPath(showID, 1))

	db := openShowChoiceDB(t, showID, "Halcyon Crossing")
	require.NoError(t, storage.SetShowPosterIndex(t.Context(), db, showID, 1))

	dir := t.TempDir()
	written, err := nfo.WriteShowCollectionFile(t.Context(), nfo.ShowWriteOptions{
		DB:         db,
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

	expectedRel, relErr := filepath.Rel(dir, cache.PosterPath(showID, 1))
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

// openShowChoiceDB spins up a fresh SQLite DB and inserts a shows row
// so SetShowPosterIndex's FK resolves. Mirrors openImageChoiceDB but
// scoped to the show-only test path.
func openShowChoiceDB(t *testing.T, showID int64, name string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "show_choices.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(t.Context(),
		`INSERT OR IGNORE INTO shows (show_id, name) VALUES (?, ?)`,
		showID, name)
	require.NoError(t, err)
	return db
}
