package imagecache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register GIF decoder for image.Decode.
	"image/jpeg"
	_ "image/png" // register PNG decoder for image.Decode.
	"io"
	"os"
	"path/filepath"
)

// UploadIndexFloor is the lowest index reserved for user-uploaded
// images. Anything below this is considered an upstream-fetched slot
// (StageMedia/Encora typically return 1-5 images, so 0..9 is the
// reserved range); user uploads land at 100, 101, 102, ... so the UI
// can distinguish "uploaded by me" from "fetched upstream" by index
// alone, no separate manifest needed.
const UploadIndexFloor = 100

// uploadJPEGQuality is the JPEG quality factor used when re-encoding
// uploaded images. 90 keeps file sizes reasonable while preserving the
// detail a poster/backdrop strip needs at thumbnail and full-size.
const uploadJPEGQuality = 90

// uploadIndexCeiling caps NextUploadPosterIndex / NextUploadBackdropIndex
// scans so a misconfigured filesystem can't spin forever. 10_000 leaves
// a 100..10099 range, far past any realistic per-show or per-recording
// upload count.
const uploadIndexCeiling = 10_000

// ErrUploadSlotExhausted is returned by NextUploadPosterIndex /
// NextUploadBackdropIndex when every slot in [UploadIndexFloor,
// uploadIndexCeiling) is occupied. Treated as a 500 by the handler —
// it indicates a runaway upload condition, not a user error.
var ErrUploadSlotExhausted = errors.New("imagecache: no free upload slot")

// NextUploadPosterIndex returns the next free index >= UploadIndexFloor
// for an uploaded poster keyed on showID. Walks 100, 101, 102, ...
// until an unused slot is found. Errors only on filesystem failure (or
// when every slot up to uploadIndexCeiling is occupied, which would
// indicate a runaway condition).
func (c *Cache) NextUploadPosterIndex(showID int64) (int, error) {
	if c.Disabled() {
		return 0, ErrDisabled
	}
	for i := UploadIndexFloor; i < uploadIndexCeiling; i++ {
		path := c.PosterPath(showID, i)
		if path == "" {
			return 0, ErrDisabled
		}
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return i, nil
		}
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	return 0, ErrUploadSlotExhausted
}

// NextUploadBackdropIndex returns the next free index >= UploadIndexFloor
// for an uploaded backdrop keyed on recordingID. Mirrors
// NextUploadPosterIndex; see that doc for the contract.
func (c *Cache) NextUploadBackdropIndex(recordingID int64) (int, error) {
	if c.Disabled() {
		return 0, ErrDisabled
	}
	for i := UploadIndexFloor; i < uploadIndexCeiling; i++ {
		path := c.BackdropPath(recordingID, i)
		if path == "" {
			return 0, ErrDisabled
		}
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return i, nil
		}
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	return 0, ErrUploadSlotExhausted
}

// SaveUploadedPoster writes raw image bytes from body to the next free
// upload slot for the given show, returning the chosen index. The
// upload is decoded via image.Decode (PNG/JPEG/GIF auto-detected) and
// re-encoded as JPEG at quality 90 so we never persist arbitrary
// upstream bytes — the on-disk shape is uniform regardless of upload
// format.
//
// Bytes are read into memory before decoding; the handler is expected
// to wrap body in http.MaxBytesReader so a malicious client can't OOM
// the process with an oversized payload.
//
// Concurrency: two simultaneous uploads against the same showID may
// pick the same index from NextUploadPosterIndex; the second write
// overwrites the first. The trade-off is intentional — uploads are
// rare, the racing window is microseconds, and a manifest-keyed lock
// would halfling the cost of the actual write. Documented for callers
// who care.
func (c *Cache) SaveUploadedPoster(
	_ context.Context, showID int64, body io.Reader,
) (int, error) {
	if c.Disabled() {
		return 0, ErrDisabled
	}
	idx, err := c.NextUploadPosterIndex(showID)
	if err != nil {
		return 0, err
	}
	if writeErr := c.writeUploadedJPEG(c.PosterPath(showID, idx), body); writeErr != nil {
		return 0, writeErr
	}
	c.Logger.Debug().
		Int64("show_id", showID).
		Int("index", idx).
		Msg("imagecache: uploaded poster stored")
	return idx, nil
}

// SaveUploadedBackdrop writes raw image bytes from body to the next
// free upload slot for the given recording, returning the chosen
// index. Mirrors SaveUploadedPoster; see that doc for the
// re-encoding/concurrency contract.
func (c *Cache) SaveUploadedBackdrop(
	_ context.Context, recordingID int64, body io.Reader,
) (int, error) {
	if c.Disabled() {
		return 0, ErrDisabled
	}
	idx, err := c.NextUploadBackdropIndex(recordingID)
	if err != nil {
		return 0, err
	}
	if writeErr := c.writeUploadedJPEG(c.BackdropPath(recordingID, idx), body); writeErr != nil {
		return 0, writeErr
	}
	c.Logger.Debug().
		Int64("recording_id", recordingID).
		Int("index", idx).
		Msg("imagecache: uploaded backdrop stored")
	return idx, nil
}

// writeUploadedJPEG decodes body, re-encodes the result as a JPEG at
// uploadJPEGQuality, and atomically writes it to dest. The parent
// directory is created if needed. A decode failure surfaces as an
// error (the caller maps it to HTTP 400 — bad upload) and no file is
// written.
//
// PNG transparency is flattened to whatever Go's jpeg encoder does by
// default (transparent pixels become black). Posters and backdrops are
// rectangular display assets so this is the right trade-off; a future
// opt-in could persist a parallel .png on top of the .jpg if the use
// case ever demands transparency.
func (c *Cache) writeUploadedJPEG(dest string, body io.Reader) error {
	if dest == "" {
		return ErrDisabled
	}
	img, _, err := image.Decode(body)
	if err != nil {
		return fmt.Errorf("decode upload: %w", err)
	}
	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, img, &jpeg.Options{Quality: uploadJPEGQuality}); encErr != nil {
		return fmt.Errorf("encode jpeg: %w", encErr)
	}
	if mkErr := os.MkdirAll(filepath.Dir(dest), dirMode); mkErr != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), mkErr)
	}
	if writeErr := os.WriteFile(dest, buf.Bytes(), fileMode); writeErr != nil {
		return fmt.Errorf("write %s: %w", dest, writeErr)
	}
	return nil
}
