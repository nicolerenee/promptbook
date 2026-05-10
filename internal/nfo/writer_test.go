package nfo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
)

// updateGolden lets developers regenerate the checked-in golden NFO when
// the writer's output intentionally changes. Run as
// `go test ./internal/nfo -update` after the change, then re-commit the
// golden alongside the diff.
//
//nolint:gochecknoglobals // standard golden-file pattern
var updateGolden = flag.Bool("update", false, "regenerate golden NFO files")

func loadMarigold(t *testing.T) encora.Recording {
	t.Helper()
	path := filepath.Join("..", "encora", "testdata", "recording_8222.json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var r encora.Recording
	require.NoError(t, json.Unmarshal(b, &r))
	return r
}

func TestWriteMarigoldGolden(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	model := nfo.FromRecording(rec)

	var buf bytes.Buffer
	require.NoError(t, nfo.Write(&buf, model))

	goldenPath := filepath.Join("testdata", "recording_8222.nfo")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, buf.Bytes(), 0o644))
		t.Logf("regenerated golden at %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden missing — run `go test -run TestWriteMarigoldGolden -update`")
	assert.Equal(t, string(want), buf.String())
}

// TestWriteRecordingFileWithImages exercises the full image-aware
// pipeline under the v2 single-file-per-slot layout: poster.jpg
// (the burned-in render output) lands as <thumb aspect="poster">,
// fanart.jpg (the raw wide image) lands as <fanart><thumb>. Paths are
// tempdir-relative so we assert via Contains rather than a golden.
func TestWriteRecordingFileWithImages(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	// Lay down poster.jpg + fanart.jpg so the writer's existence checks
	// resolve true.
	writeFakeImage(t, cache.RecordingPosterPath(rec.ID))
	writeFakeImage(t, cache.RecordingFanartPath(rec.ID))

	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{Cache: cache},
	)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(folder, "movie.nfo"), written)

	got, err := os.ReadFile(written)
	require.NoError(t, err)

	expectedPosterRel, relErr := filepath.Rel(folder, cache.RecordingPosterPath(rec.ID))
	require.NoError(t, relErr)
	expectedFanartRel, relErr := filepath.Rel(folder, cache.RecordingFanartPath(rec.ID))
	require.NoError(t, relErr)
	assert.Contains(t, string(got),
		`<thumb aspect="poster">`+filepath.ToSlash(expectedPosterRel)+`</thumb>`)
	assert.Contains(t, string(got),
		`<thumb>`+filepath.ToSlash(expectedFanartRel)+`</thumb>`)
}

// TestWriteRecordingFileNoCache verifies that with imaging disabled the
// NFO simply omits the <thumb> / <fanart> elements rather than emitting
// a broken reference.
func TestWriteRecordingFileNoCache(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(t.Context(), folder, rec, nfo.WriteOptions{})
	require.NoError(t, err)

	got, err := os.ReadFile(written)
	require.NoError(t, err)
	assert.NotContains(t, string(got), `<thumb`)
	assert.NotContains(t, string(got), `<fanart>`)
}

// TestWriteRecordingFileFanartOnly verifies that when only fanart.jpg
// exists (no poster.jpg yet — overlay-render hasn't happened) the
// writer emits the fanart hint and omits the poster element.
func TestWriteRecordingFileFanartOnly(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	fanartPath := cache.RecordingFanartPath(rec.ID)
	writeFakeImage(t, fanartPath)

	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{Cache: cache},
	)
	require.NoError(t, err)
	got, err := os.ReadFile(written)
	require.NoError(t, err)

	expectedRel, relErr := filepath.Rel(folder, fanartPath)
	require.NoError(t, relErr)
	assert.Contains(t, string(got), `<thumb>`+filepath.ToSlash(expectedRel)+`</thumb>`)
	assert.NotContains(t, string(got), `<thumb aspect="poster">`)
}

// TestNFO_WithPublicURL exercises the URL emission path: with
// PublicURL set, the writer must emit absolute /images/* URLs for the
// movie poster, fanart, and every actor's headshot — no local sibling
// paths, no Cache-resolved paths, regardless of whether the cache has
// the files on disk.
func TestNFO_WithPublicURL(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)

	// Trailing-slash on the public URL should be stripped so the
	// resulting URLs don't have "//images" — verify with both shapes.
	tests := []struct {
		name      string
		publicURL string
	}{
		{name: "no trailing slash", publicURL: "https://promptbook.example.com"},
		{name: "trailing slash", publicURL: "https://promptbook.example.com/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Each parallel subtest gets its own folder so the two
			// goroutines don't race on writing the same movie.nfo.
			folder := t.TempDir()

			written, err := nfo.WriteRecordingFile(
				t.Context(), folder, rec,
				nfo.WriteOptions{PublicURL: tt.publicURL},
			)
			require.NoError(t, err)

			body, err := os.ReadFile(written)
			require.NoError(t, err)
			got := string(body)

			// Movie poster: <thumb aspect="poster">URL</thumb> at the
			// recording's poster slot. WriteOptions had no Cache so
			// the placeholder fall-through stamps "?v=generated".
			assert.Contains(
				t,
				got,
				`<thumb aspect="poster">https://promptbook.example.com/images/recordings/90100222/poster.jpg?v=generated</thumb>`,
			)
			// Fanart wraps a <thumb> child.
			assert.Contains(
				t,
				got,
				`<thumb>https://promptbook.example.com/images/recordings/90100222/fanart.jpg?v=generated</thumb>`,
			)
			// First cast entry on the Marigold fixture is Avery Morrison
			// James (performer id 90001001, role Marigold).
			assert.Contains(t, got,
				`<thumb>https://promptbook.example.com/images/actors/90001001.jpg?v=generated</thumb>`)
			// And the last named cast entry is Marisol Vandermeer (id 90001018).
			assert.Contains(t, got,
				`<thumb>https://promptbook.example.com/images/actors/90001018.jpg?v=generated</thumb>`)
			// No double-slash anywhere.
			assert.NotContains(t, got, `//images/`)
		})
	}
}

// TestNFO_PublicURLCacheBust covers the `?v={mtime}` cache-buster
// emission path: when opts.Cache is set and a slot file is on disk,
// the writer must append `?v={file mtime in unix seconds}` to the
// emitted URL. Files that aren't on disk yet emit the bare URL (no
// `?v=0` suffix) so the wire shape distinguishes "no image yet" from
// "image with timestamp 0".
func TestNFO_PublicURLCacheBust(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	// Lay down each slot with a known mtime so we can assert against
	// the exact unix-second value the writer reads. Use distinct
	// mtimes per slot so a cross-wired implementation surfaces in
	// the diff.
	posterMtime := int64(1_700_000_001)
	fanartMtime := int64(1_700_000_002)
	bannerMtime := int64(1_700_000_003)
	headshot1380Mtime := int64(1_700_000_004)
	headshot3003Mtime := int64(1_700_000_005)
	writeImageWithMtime(t, cache.RecordingPosterPath(rec.ID), posterMtime)
	writeImageWithMtime(t, cache.RecordingFanartPath(rec.ID), fanartMtime)
	writeImageWithMtime(t, cache.ShowBannerPath(rec.Metadata.ShowID), bannerMtime)
	writeImageWithMtime(t, cache.HeadshotPath(90001001), headshot1380Mtime)
	writeImageWithMtime(t, cache.HeadshotPath(90001018), headshot3003Mtime)

	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{
			Cache:     cache,
			PublicURL: "https://promptbook.example.com",
		},
	)
	require.NoError(t, err)

	body, err := os.ReadFile(written)
	require.NoError(t, err)
	got := string(body)

	// Each URL carries `?v={its-own-mtime}`. The XML encoder escapes
	// ? as ? in chardata (it's a literal); the encoded form is the
	// same plain string here since `?` is not a reserved character in
	// XML chardata.
	assert.Contains(t, got, fmt.Sprintf(
		`<thumb aspect="poster">https://promptbook.example.com/images/recordings/90100222/poster.jpg?v=%d</thumb>`,
		posterMtime,
	))
	assert.Contains(t, got, fmt.Sprintf(
		`<thumb>https://promptbook.example.com/images/recordings/90100222/fanart.jpg?v=%d</thumb>`,
		fanartMtime))
	// Show banner appears in <set><thumb> + <set><fanart><thumb>.
	bannerURL := fmt.Sprintf(
		`https://promptbook.example.com/images/shows/%d/banner.jpg?v=%d`,
		rec.Metadata.ShowID, bannerMtime)
	assert.Contains(t, got, "<thumb>"+bannerURL+"</thumb>")
	// Headshots carry the per-actor mtime.
	assert.Contains(t, got, fmt.Sprintf(
		`<thumb>https://promptbook.example.com/images/actors/90001001.jpg?v=%d</thumb>`,
		headshot1380Mtime))
	assert.Contains(t, got, fmt.Sprintf(
		`<thumb>https://promptbook.example.com/images/actors/90001018.jpg?v=%d</thumb>`,
		headshot3003Mtime))
}

// TestNFO_PublicURLNoCacheBareURLs covers the negative path:
// PublicURL set, Cache nil → bare URLs without any `?v=` suffix.
// Same as TestNFO_WithPublicURL above but spelled out so a future
// regression that sneaks `?v=0` into the wire shape fails here.
func TestNFO_PublicURLNoCacheBareURLs(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	folder := t.TempDir()

	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{PublicURL: "https://promptbook.example.com"},
	)
	require.NoError(t, err)
	body, err := os.ReadFile(written)
	require.NoError(t, err)
	got := string(body)

	// Cache nil → every mtime is 0 and versionedURL stamps
	// `?v=generated` so media servers know the URL points at a
	// synthesized placeholder. No `?v={number}` should leak through.
	assert.NotContains(t, got, "?v=1") // any unix-second mtime value
	// Generated marker present on every URL.
	assert.Contains(
		t,
		got,
		`<thumb aspect="poster">https://promptbook.example.com/images/recordings/90100222/poster.jpg?v=generated</thumb>`,
	)
	assert.Contains(
		t,
		got,
		`<thumb>https://promptbook.example.com/images/recordings/90100222/fanart.jpg?v=generated</thumb>`,
	)
	assert.Contains(t, got,
		`<thumb>https://promptbook.example.com/images/actors/90001001.jpg?v=generated</thumb>`)
}

// TestNFO_PublicURLPartialMtime covers the mixed case: PublicURL set,
// Cache set, but only some slot files are on disk. The slots that
// have mtimes get `?v=`; the missing ones stay bare. Important
// because a freshly-imported recording typically has poster +
// fanart but no headshots yet.
func TestNFO_PublicURLPartialMtime(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	// Only poster on disk; fanart, banner, headshots all missing.
	posterMtime := int64(1_700_000_010)
	writeImageWithMtime(t, cache.RecordingPosterPath(rec.ID), posterMtime)

	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{
			Cache:     cache,
			PublicURL: "https://promptbook.example.com",
		},
	)
	require.NoError(t, err)
	body, err := os.ReadFile(written)
	require.NoError(t, err)
	got := string(body)

	assert.Contains(t, got, fmt.Sprintf(
		`<thumb aspect="poster">https://promptbook.example.com/images/recordings/90100222/poster.jpg?v=%d</thumb>`,
		posterMtime,
	))
	// Fanart placeholder — file isn't on disk so versionedURL stamps
	// the "generated" marker.
	assert.Contains(
		t,
		got,
		`<thumb>https://promptbook.example.com/images/recordings/90100222/fanart.jpg?v=generated</thumb>`,
	)
	// Headshot 90001001 placeholder — file isn't on disk.
	assert.Contains(t, got,
		`<thumb>https://promptbook.example.com/images/actors/90001001.jpg?v=generated</thumb>`)
}

// writeImageWithMtime writes a 1-byte placeholder at path and
// sets its mtime to the given unix-seconds value. Lets cache-bust
// tests assert against an exact `?v=<mtime>` string instead of a
// wall-clock-tied near-now value.
func writeImageWithMtime(t *testing.T, path string, mtime int64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
	ts := time.Unix(mtime, 0)
	require.NoError(t, os.Chtimes(path, ts, ts))
}

// TestNFO_NoPublicURL is the symmetric guard: with PublicURL empty
// the writer must NOT emit URL-flavoured thumbs. Movie + fanart fall
// back to local sibling paths via Cache (or get omitted entirely
// when the cache is also disabled), and every <actor> element ends
// at <order> with no <thumb> child.
func TestNFO_NoPublicURL(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	folder := t.TempDir()

	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{}, // no PublicURL, no Cache.
	)
	require.NoError(t, err)

	body, err := os.ReadFile(written)
	require.NoError(t, err)
	got := string(body)

	// No actor thumbs anywhere — Jellyfin doesn't have a local-fallback
	// convention, so emitting nothing is the right thing.
	assert.NotContains(t, got, `/images/actors/`)
	// No URL-flavoured movie thumbs either.
	assert.NotContains(t, got, `/images/recordings/`)
	assert.NotContains(t, got, `<thumb`)
	assert.NotContains(t, got, `<fanart>`)

	// Spot-check that an actor block ends at <order> and contains
	// only name/role/order — exactly the shape the golden test
	// codifies.
	assert.Contains(t, got,
		"<actor>\n    <name>Avery Morrison</name>\n"+
			"    <role>Marigold</role>\n    <order>1</order>\n  </actor>")
}

// writeFakeImage drops a 1-byte placeholder at path, creating parents.
// Keeps tests cheap — the writer only stat()s these files, never reads.
func writeFakeImage(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
}

// zerologNop returns a zero-value zerolog.Logger — the package's
// documented nop logger contract — so cache construction in tests
// doesn't print to stderr.
func zerologNop() zerolog.Logger { return zerolog.Nop() }

func TestFromRecordingPartialDate(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t) // Marigold's day is unknown → premiered should be empty.
	model := nfo.FromRecording(rec)

	assert.Empty(t, model.Premiered, "day_known=false ⇒ omit premiered")
	assert.Equal(t, "2009", model.Year)
	assert.Equal(t, "Marigold Junction — Broadway — December 2009", model.Title)
	assert.Equal(t, "Marigold Junction", model.Set.Name)
}

func TestFromRecordingFullDate(t *testing.T) {
	t.Parallel()

	full := encora.Recording{
		ID:   90118317,
		Show: "Tideline Manor",
		Tour: "First US National Tour",
		Date: encora.Date{FullDate: "2024-01-21", MonthKnown: true, DayKnown: true},
		Cast: []encora.CastEntry{
			{
				Performer: encora.Performer{Name: "Riley Chen"},
				Character: encora.Character{Name: "Tideline Manor", Order: 1},
			},
		},
		Notes: "Great recording.",
	}
	model := nfo.FromRecording(full)
	assert.Equal(t, "2024-01-21", model.Premiered)
	assert.Equal(t, "Tideline Manor — First US National Tour — 2024-01-21", model.Title)
	require.Len(t, model.UniqueIDs, 1)
	assert.Equal(t, "encora", model.UniqueIDs[0].Type)
	assert.Equal(t, "90118317", model.UniqueIDs[0].Value)
	assert.True(t, model.UniqueIDs[0].Default)
}

func TestWriteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rec := loadMarigold(t)
	path, err := nfo.WriteFile(dir, nfo.FromRecording(rec))
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), `<?xml version="1.0"`)
	assert.Contains(t, string(got), `<uniqueid type="encora" default="true">90100222</uniqueid>`)
}

// TestWriteRecordingFileEmitsOneUniqueIDPerExternalID covers commit
// 4's framework hook: when WriteOptions.ExternalIDs is set, the
// writer emits one <uniqueid> per row instead of the legacy
// single-Encora shape. The Encora row stays default="true" so
// Jellyfin keys dedupe on it; other providers carry their type
// strings verbatim.
func TestWriteRecordingFileEmitsOneUniqueIDPerExternalID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rec := loadMarigold(t)
	_, err := nfo.WriteRecordingFile(
		context.Background(),
		dir,
		rec,
		nfo.WriteOptions{
			ExternalIDs: []externalids.ExternalID{
				{Provider: externalids.ProviderEncora, ExternalID: "90100222"},
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
			},
		},
	)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "movie.nfo"))
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, `<uniqueid type="encora" default="true">90100222</uniqueid>`)
	assert.Contains(t, body, `<uniqueid type="tmdb" default="false">90181637</uniqueid>`)
	assert.Contains(t, body, `<uniqueid type="imdb" default="false">tt99999999</uniqueid>`)
}

// TestWriteRecordingFileFallbackEncoraWhenListEmpty asserts the legacy
// single-Encora shape stays intact when the caller doesn't supply
// ExternalIDs — preserves the wire format for every existing
// consumer that hasn't been re-wired yet.
func TestWriteRecordingFileFallbackEncoraWhenListEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rec := loadMarigold(t)
	_, err := nfo.WriteRecordingFile(context.Background(), dir, rec, nfo.WriteOptions{})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "movie.nfo"))
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, `<uniqueid type="encora" default="true">90100222</uniqueid>`)
	assert.NotContains(t, body, `<uniqueid type="tmdb"`)
	assert.NotContains(t, body, `<uniqueid type="imdb"`)
}

// TestFormatRolePrefixesStatus asserts the role string carries the
// understudy/swing/etc. abbreviation as a leading token, matching the
// "U/s Elsa" convention the legacy hand-rolled writer used and that
// Jellyfin echoes verbatim into the cast list.
func TestFormatRoleWithStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status *encora.CastStatus
		role   string
		want   string
	}{
		{"no status", nil, "Elsa", "Elsa"},
		{"empty abbrev", &encora.CastStatus{}, "Elsa", "Elsa"},
		{"understudy", &encora.CastStatus{Abbreviation: "u/s"}, "Elsa", "u/s Elsa"},
		{"alternate", &encora.CastStatus{Abbreviation: "alt"}, "Companion", "alt Companion"},
		{"swing", &encora.CastStatus{Abbreviation: "s/w"}, "", "s/w"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := encora.Recording{
				ID:   1,
				Show: "Velvet Antlers",
				Cast: []encora.CastEntry{{
					Performer: encora.Performer{ID: 99, Name: "Test Performer"},
					Character: encora.Character{Name: tt.role, Order: 1},
					Status:    tt.status,
				}},
			}
			model := nfo.FromRecording(rec)
			require.Len(t, model.Actors, 1)
			assert.Equal(t, tt.want, model.Actors[0].Role)
		})
	}
}
