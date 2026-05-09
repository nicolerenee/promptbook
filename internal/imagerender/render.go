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
	title, subtitle := splitOverlay(overlayText)

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

	dst := r.compose(src, title, subtitle, style)

	if writeErr := writeJPEGAtomic(destPath, dst); writeErr != nil {
		return fmt.Errorf("imagerender: write %s: %w", destPath, writeErr)
	}
	r.Logger.Debug().
		Int64("recording_id", recordingID).
		Str("dest", destPath).
		Msg("imagerender: poster.jpg written")
	return nil
}

// autoOverlayText returns the default "Show · Tour · Date · Master"
// label burned into rendered.jpg when the user hasn't supplied an
// override. Returns the empty string when loaded is nil so the caller
// can fall through to whatever text the choice has.
func autoOverlayText(loaded *storage.LoadedRecording) string {
	if loaded == nil {
		return ""
	}
	r := loaded.Recording
	title := strings.TrimSpace(r.Show)
	parts := []string{}
	if t := strings.TrimSpace(r.Tour); t != "" {
		parts = append(parts, t)
	}
	if d := smartDate(r.Date); d != "" {
		parts = append(parts, d)
	}
	if m := strings.TrimSpace(r.Master); m != "" {
		parts = append(parts, m)
	}
	subtitle := strings.Join(parts, " · ")
	switch {
	case title == "" && subtitle == "":
		return ""
	case title == "":
		return subtitle
	case subtitle == "":
		return title
	default:
		return title + "\n" + subtitle
	}
}

// splitOverlay turns one overlay-text blob into a (title, subtitle)
// pair. An explicit newline wins; otherwise the first " · " separator
// splits the line into a title (everything before) and subtitle
// (everything after). When neither delimiter is present the whole
// string becomes the title and the subtitle is empty.
func splitOverlay(text string) (string, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if before, after, found := strings.Cut(text, "\n"); found {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	if before, after, found := strings.Cut(text, " · "); found {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	return text, ""
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
