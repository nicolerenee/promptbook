package nfo_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/storage"
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
// pipeline: a populated cache + DB choice, a rendered.jpg sibling
// taking precedence over the raw selected backdrop, and the resulting
// <thumb aspect="poster"> + <fanart><thumb> hints. Paths are tempdir-
// relative so we assert via Contains rather than a checked-in golden.
func TestWriteRecordingFileWithImages(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	// Lay down a synthetic poster, raw backdrop, and rendered backdrop
	// so the writer's existence checks resolve true. Index 1 verifies
	// ResolvePoster/Backdrop are honored over the default 0.
	posterIdx, backdropIdx := 1, 1
	writeFakeImage(t, cache.PosterPath(rec.Metadata.ShowID, posterIdx))
	writeFakeImage(t, cache.BackdropPath(rec.ID, backdropIdx))
	// rendered.jpg sits beside the indexed backdrops; its presence
	// should make it win over <id>/1.jpg.
	renderedPath := filepath.Join(filepath.Dir(cache.BackdropPath(rec.ID, backdropIdx)), "rendered.jpg")
	writeFakeImage(t, renderedPath)

	db := openImageChoiceDB(t, rec)
	require.NoError(t, storage.SetPosterIndex(t.Context(), db, rec.ID, posterIdx))
	require.NoError(t, storage.SetBackdropIndex(t.Context(), db, rec.ID, backdropIdx))

	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{DB: db, Cache: cache},
	)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(folder, "movie.nfo"), written)

	got, err := os.ReadFile(written)
	require.NoError(t, err)

	// Sanity-check the image element shape before pinning to a golden.
	// Both paths are .. -relative because the cache lives in a sibling
	// tempdir, not under folder. Forward slashes always.
	expectedPosterRel, relErr := filepath.Rel(folder, cache.PosterPath(rec.Metadata.ShowID, posterIdx))
	require.NoError(t, relErr)
	expectedFanartRel, relErr := filepath.Rel(folder, renderedPath)
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

// TestWriteRecordingFileBackdropFallback verifies that when no
// rendered.jpg exists, the writer falls back to the raw indexed
// backdrop rather than dropping the fanart entirely.
func TestWriteRecordingFileBackdropFallback(t *testing.T) {
	t.Parallel()

	rec := loadMarigold(t)
	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerologNop())

	rawBackdrop := cache.BackdropPath(rec.ID, 0)
	writeFakeImage(t, rawBackdrop)
	// No rendered.jpg, no poster → only fanart should land.

	db := openImageChoiceDB(t, rec)
	folder := t.TempDir()
	written, err := nfo.WriteRecordingFile(
		t.Context(), folder, rec,
		nfo.WriteOptions{DB: db, Cache: cache},
	)
	require.NoError(t, err)
	got, err := os.ReadFile(written)
	require.NoError(t, err)

	expectedRel, relErr := filepath.Rel(folder, rawBackdrop)
	require.NoError(t, relErr)
	assert.Contains(t, string(got), `<thumb>`+filepath.ToSlash(expectedRel)+`</thumb>`)
	assert.NotContains(t, string(got), `<thumb aspect="poster">`)
}

// writeFakeImage drops a 1-byte placeholder at path, creating parents.
// Keeps tests cheap — the writer only stat()s these files, never reads.
func writeFakeImage(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
}

// openImageChoiceDB spins up a fresh SQLite DB with migrations applied
// and inserts the parent show + recording rows for rec.ID so the
// recording_image_choices FK constraint is satisfied. Used by tests
// that exercise GetImageChoice / SetPosterIndex / SetBackdropIndex
// through the NFO writer.
func openImageChoiceDB(t *testing.T, rec encora.Recording) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "image_choices.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Seed parent rows. INSERT OR IGNORE makes the helper safe across
	// multiple invocations of the same test setup.
	_, err = db.ExecContext(t.Context(),
		`INSERT OR IGNORE INTO shows (show_id, name) VALUES (?, ?)`,
		rec.Metadata.ShowID, rec.Show)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		`INSERT OR IGNORE INTO recordings
			(recording_id, show_id, tour, date_full, raw_json)
		 VALUES (?, ?, ?, ?, ?)`,
		rec.ID, rec.Metadata.ShowID, rec.Tour, rec.Date.FullDate, "{}")
	require.NoError(t, err)
	return db
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
