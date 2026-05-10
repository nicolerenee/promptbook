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
	"database/sql"
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
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// Renderer composites the raw poster source with a user-curated text
// overlay and writes the result to poster.jpg under the recording's
// directory.
type Renderer struct {
	DB     *sql.DB
	Cache  *imagecache.Cache
	Logger zerolog.Logger
}

// New constructs a Renderer. Returns nil when cache is nil or
// disabled — the caller's nil-check skips regeneration in that mode.
func New(db *sql.DB, cache *imagecache.Cache, logger zerolog.Logger) *Renderer {
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
	autoText := autoOverlayText(loaded)
	overlayText := choice.ResolveOverlayText(autoText)
	rows := splitOverlay(overlayText)

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

// autoOverlayText returns the default label burned into the recording
// poster when the user hasn't supplied an override.
//
// Layout (per the brand spec):
//
//	Line 1 (top, smaller):  date
//	Line 2 (middle, larger): tour
//	Line 3 (bottom, smaller): "Venue, City"
//
// Empty fields collapse cleanly. If every line is empty (the recording
// has no metadata at all) the show name takes line 2 as a last-resort
// fallback so the band never renders blank.
func autoOverlayText(loaded *storage.LoadedRecording) string {
	if loaded == nil {
		return ""
	}
	r := loaded.Recording
	date := smartDate(r.Date)
	tour := strings.TrimSpace(r.Tour)
	venue := strings.TrimSpace(r.Metadata.Venue)
	city := strings.TrimSpace(r.Metadata.City)
	location := joinSep(venue, city, ", ")
	if date == "" && tour == "" && location == "" {
		// Nothing identifying — fall back to the show name on the
		// middle row so the band has at least one readable line.
		tour = strings.TrimSpace(r.Show)
	}
	// Always emit three "\n"-separated rows (some may be empty);
	// splitOverlay preserves position so an empty row stays empty
	// rather than collapsing the layout. compose filters empties at
	// draw time.
	return date + "\n" + tour + "\n" + location
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

// splitOverlay returns the three rows the renderer expects: date,
// tour, location. An overlay-text blob with three "\n"-separated
// segments hits the happy path. Two segments are interpreted as
// tour + location (the "no date known" case). One segment lands on
// the middle row alone. Trailing or leading empty rows preserve their
// position so the user's chosen layout doesn't get reshuffled.
func splitOverlay(text string) []string {
	out := []string{"", "", ""}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return out
	}
	parts := strings.Split(text, "\n")
	const (
		idxEyebrow   = 0
		idxTitle     = 1
		idxCaption   = 2
		twoLineParts = 2
	)
	switch len(parts) {
	case 1:
		out[idxTitle] = strings.TrimSpace(parts[0])
	case twoLineParts:
		out[idxTitle] = strings.TrimSpace(parts[0])
		out[idxCaption] = strings.TrimSpace(parts[1])
	default:
		out[idxEyebrow] = strings.TrimSpace(parts[0])
		out[idxTitle] = strings.TrimSpace(parts[1])
		out[idxCaption] = strings.TrimSpace(parts[2])
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
