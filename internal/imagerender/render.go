// Package imagerender produces the burned-in poster image (poster.jpg)
// the NFO writer points Jellyfin/Plex at as <thumb aspect="poster">.
// The raw poster source lives at recordings/<id>/poster-src.jpg;
// poster.jpg sits next to it with the user-curated text band
// composited at the bottom.
//
// The composite is intentionally playbill-inspired but flipped: a
// near-black band runs across the bottom 14 % of the image, and the
// user-curated overlay text renders inside the band in an uppercase
// serif. Default text color is a muted gold (#c8a14a) — easy to swap
// once the user dials in their preference. Style overrides can be
// supplied per-recording via storage.SetOverlayStyle (a JSON blob).
//
// All compositing is pure Go (image/draw + golang.org/x/image), so the
// renderer ships without cgo and runs on any Go-supported platform.
package imagerender

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// Renderer composites the raw poster source with a user-curated text
// overlay and writes the result to poster.jpg under the recording's
// directory.
type Renderer struct {
	DB     *ent.Client
	Cache  *imagecache.Cache
	Logger zerolog.Logger
}

// New constructs a Renderer. Returns nil when cache is nil or
// disabled — the caller's nil-check skips regeneration in that mode.
func New(db *ent.Client, cache *imagecache.Cache, logger zerolog.Logger) *Renderer {
	if cache == nil || cache.Disabled() {
		return nil
	}
	return &Renderer{DB: db, Cache: cache, Logger: logger}
}

// Regenerate composes poster.jpg for the recording from poster-src.jpg
// and the overlay text + style stored in recording_image_choices.
// Idempotent — calling it repeatedly for the same source + style
// produces a byte-identical file. No-op when the recording has no
// poster-src.jpg on disk.
func (r *Renderer) Regenerate(ctx context.Context, recordingID int64) error {
	if r == nil {
		return nil
	}

	choice, err := storage.GetImageChoice(ctx, r.DB, recordingID)
	if err != nil {
		return fmt.Errorf("imagerender: load image choice for %d: %w", recordingID, err)
	}

	if !r.Cache.HasRecordingPosterSrc(recordingID) {
		// Nothing to render yet — refresh-recording-images hasn't
		// pulled a poster source or the user uploaded one. Soft no-op.
		r.Logger.Debug().
			Int64("recording_id", recordingID).
			Msg("imagerender: no poster-src on disk; skipping")
		return nil
	}
	srcPath := r.Cache.RecordingPosterSrcPath(recordingID)
	destPath := r.Cache.RecordingPosterPath(recordingID)

	// Burn-in opt-out: copy poster-src.jpg verbatim to poster.jpg so
	// the NFO writer's <thumb> still resolves but the resulting file
	// carries no compositing. We stream bytes (no decode/re-encode) so
	// the user's chosen image lands on disk lossless.
	if choice.OverlayDisabled {
		if copyErr := copyFileAtomic(srcPath, destPath); copyErr != nil {
			return fmt.Errorf("imagerender: copy raw %s -> %s: %w", srcPath, destPath, copyErr)
		}
		r.Logger.Debug().
			Int64("recording_id", recordingID).
			Str("dest", destPath).
			Msg("imagerender: overlay disabled; poster.jpg is raw copy")
		return nil
	}

	loaded, err := storage.LoadRecording(ctx, r.DB, recordingID)
	if err != nil && !errors.Is(err, storage.ErrRecordingNotFound) {
		return fmt.Errorf("imagerender: load recording %d: %w", recordingID, err)
	}
	// Auto path emits the three rows directly so we don't round-trip
	// through a "\n"-joined string. The override path still parses the
	// user-typed string (single textarea on the SPA) into 3 slots via
	// splitOverlay; positions still map line-by-line.
	rows := autoOverlayRows(loaded)
	if choice.OverlayTextOverride != nil {
		rows = splitOverlay(*choice.OverlayTextOverride)
	}

	style := DefaultStyle
	if choice.OverlayStyleJSON != nil && *choice.OverlayStyleJSON != "" {
		merged, mergeErr := mergeStyle(DefaultStyle, *choice.OverlayStyleJSON)
		if mergeErr != nil {
			r.Logger.Warn().
				Err(mergeErr).
				Int64("recording_id", recordingID).
				Msg("imagerender: bad overlay style JSON; using default")
		} else {
			style = merged
		}
	}

	src, err := decodeJPEG(srcPath)
	if err != nil {
		return fmt.Errorf("imagerender: decode %s: %w", srcPath, err)
	}

	dst := r.compose(src, rows, style)

	if writeErr := writeJPEGAtomic(destPath, dst); writeErr != nil {
		return fmt.Errorf("imagerender: write %s: %w", destPath, writeErr)
	}
	r.Logger.Debug().
		Int64("recording_id", recordingID).
		Str("dest", destPath).
		Msg("imagerender: poster.jpg written")
	return nil
}

// autoOverlayRows returns the three default rows burned into the
// recording poster when the user hasn't supplied an override, in
// fixed slot order: [date, tour, location]. Empty strings stand in
// for missing fields — compose() preserves slot position by drawing
// at fixed band fractions, so a recording missing the location
// renders the date + tour at the same Y coordinates as a complete
// 3-row recording would.
//
// Falls back to the show name on the tour row when every other field
// is empty so the band never renders entirely blank.
func autoOverlayRows(loaded *storage.LoadedRecording) []string {
	rows := make([]string, overlayRowCount)
	if loaded == nil {
		return rows
	}
	r := loaded.Recording
	rows[0] = smartDate(r.Date)
	rows[1] = strings.TrimSpace(r.Tour)
	rows[2] = joinSep(strings.TrimSpace(r.Metadata.Venue),
		strings.TrimSpace(r.Metadata.City), ", ")
	if rows[0] == "" && rows[1] == "" && rows[2] == "" {
		// Nothing identifying — fall back to the show name on the
		// title row so the band has at least one readable line.
		rows[1] = strings.TrimSpace(r.Show)
	}
	return rows
}

// joinSep returns "a<sep>b" when both parts are non-empty, the
// non-empty one alone when the other is, or "" when both are. Used
// to format the two-field title and subtitle without leaving stray
// separators on partial data.
func joinSep(a, b, sep string) string {
	switch {
	case a == "" && b == "":
		return ""
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + sep + b
	}
}

// splitOverlay parses a user-typed override string into the three
// rows compose() expects, in fixed slot order: [date, tour, location].
// Used only for the override path — the auto path goes straight to
// autoOverlayRows() without round-tripping through a "\n"-joined
// string. The split is strictly positional: line 1 = date, line 2
// = tour, line 3 = venue. To leave a slot empty, type a blank line
// for it (e.g., "\nMY TITLE\nMY VENUE" puts text on the title +
// caption rows only). Trailing missing rows are treated as empty so
// "DATE\nTOUR" parses cleanly with an empty caption slot.
func splitOverlay(text string) []string {
	out := []string{"", "", ""}
	if text == "" {
		return out
	}
	parts := strings.Split(text, "\n")
	for i := range overlayRowCount {
		if i >= len(parts) {
			break
		}
		out[i] = strings.TrimSpace(parts[i])
	}
	return out
}

// smartDate mirrors the rename engine + nfo writer's {Date} token.
// Reproduced here to avoid an internal/internal dependency.
func smartDate(d encora.Date) string {
	full := d.FullDate
	if len(full) < dateYearLen {
		return full
	}
	switch {
	case !d.MonthKnown:
		return full[:dateYearLen]
	case !d.DayKnown:
		if len(full) >= dateMonthLen {
			return full[:dateMonthLen]
		}
		return full
	default:
		if len(full) >= dateFullLen {
			return full[:dateFullLen]
		}
		return full
	}
}

// decodeJPEG reads + decodes a JPEG file from disk. Wraps both error
// paths so callers see one error class.
func decodeJPEG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return img, nil
}

// writeJPEGAtomic encodes img to a sibling .tmp file and renames it
// over dest on success. A partial render never overwrites a previously
// good file.
func writeJPEGAtomic(dest string, img image.Image) error {
	if dest == "" {
		return errors.New("imagerender: empty destination path")
	}
	if mkErr := os.MkdirAll(filepath.Dir(dest), dirMode); mkErr != nil {
		return fmt.Errorf("mkdir: %w", mkErr)
	}
	tmp := dest + ".tmp"
	// Best-effort cleanup of any stale tmp from a prior crash.
	if _, statErr := os.Stat(tmp); statErr == nil {
		_ = os.Remove(tmp)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if encErr := jpeg.Encode(f, img, &jpeg.Options{Quality: jpegQuality}); encErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encode: %w", encErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", closeErr)
	}
	if renameErr := os.Rename(tmp, dest); renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}

// copyFileAtomic streams src to a sibling .tmp file and renames it
// over dest on success. Used by the overlay-disabled path so the raw
// JPEG bytes flow through unchanged — no decode/re-encode, no quality
// loss. Mirrors writeJPEGAtomic's tmp-then-rename contract so callers
// never observe a torn file.
func copyFileAtomic(src, dest string) error {
	if mkErr := os.MkdirAll(filepath.Dir(dest), dirMode); mkErr != nil {
		return fmt.Errorf("mkdir: %w", mkErr)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = in.Close() }()

	tmp := dest + ".tmp"
	if _, statErr := os.Stat(tmp); statErr == nil {
		_ = os.Remove(tmp)
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", closeErr)
	}
	if renameErr := os.Rename(tmp, dest); renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}

// Constants for the writer + JPEG encoder. Pulled out so the magic
// numbers don't trip mnd lint.
const (
	dirMode      fs.FileMode = 0o750
	fileMode     fs.FileMode = 0o600
	jpegQuality              = 90
	dateYearLen              = 4
	dateMonthLen             = 7
	dateFullLen              = 10
)
