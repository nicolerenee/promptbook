// Package imagecache stores the user's chosen poster, fanart, and
// headshot images on local disk so promptbook detail pages survive
// upstream deletions and don't hot-link to third-party CDNs.
//
// The cache is file-system-backed — there is no SQLite manifest. File
// existence on disk is the only source of truth; HasX methods are
// cheap os.Stat calls. Disabled mode (Root == "") is a hard no-op:
// every method returns the empty/false zero value, callers don't have
// to nil-check the cache itself.
//
// Disk layout (canonical, single-file-per-slot v2):
//
//	<Root>/actors/<actor_id>.jpg                — single headshot per actor
//	<Root>/shows/<show_id>/banner.jpg           — show's chosen image
//	<Root>/recordings/<recording_id>/
//	    fanart.jpg                              — raw chosen wide image
//	    poster.jpg                              — vertical poster WITH burned-in overlay
//	    poster-src.jpg                          — raw source for poster.jpg
//
// File extension is always .jpg. StageMedia returns JPEG and Encora's
// screen grabs are JPEG by convention; if PNG/WEBP support ever
// becomes necessary we'll add a per-source extension dance and migrate
// in place.
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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "golang.org/x/image/webp" // register WebP — StageMedia serves WebP under .jpg URLs.

	"github.com/rs/zerolog"
)

// Subdirectory names under Root. Exposed as constants so the server's
// /images/* file server and the cache writer agree on layout.
const (
	actorsDir     = "actors"
	showsDir      = "shows"
	recordingsDir = "recordings"

	// Per-slot filenames inside the per-entity subdirectories.
	bannerFile    = "banner.jpg"
	fanartFile    = "fanart.jpg"
	posterFile    = "poster.jpg"
	posterSrcFile = "poster-src.jpg"

	// fileExt is the extension every cached image is saved with. See
	// the package doc for the rationale.
	fileExt = ".jpg"

	// dirMode is the permission mode used when MkdirAll creates a new
	// subdirectory. 0o750 keeps the cache readable by the user + group
	// that owns the process, but not world-readable.
	dirMode = 0o750
	// fileMode is the permission mode used when WriteFile lays down a
	// cached image. Tighter than dirMode because the file shouldn't be
	// group-writable.
	fileMode = 0o600

	// fetchTimeout caps a single image download. Generous enough for
	// large CDN images on a slow link, tight enough that a stuck
	// connection can't wedge an entire sync run.
	fetchTimeout = 60 * time.Second

	// maxImageBytes is the largest single image we'll persist. 50 MiB
	// is well above any realistic poster/headshot size; a body bigger
	// than this almost certainly isn't an image and signals an
	// upstream error page or a misconfigured URL.
	maxImageBytes = 50 * 1024 * 1024

	// uploadJPEGQuality is the JPEG quality factor used when re-encoding
	// uploaded images. 90 keeps file sizes reasonable while preserving
	// the detail a poster/banner needs at thumbnail and full-size.
	uploadJPEGQuality = 90
)

// ErrDisabled is returned by Fetch* methods when the cache is disabled
// (Root == ""). Callers should treat it as a soft signal — caching is
// best-effort, never load-bearing.
var ErrDisabled = errors.New("imagecache: disabled (no root configured)")

// ErrNotImage is returned by Fetch* methods when the upstream response
// did not look like an image (Content-Type didn't start with image/,
// or the body was empty). The error includes the URL for triage.
var ErrNotImage = errors.New("imagecache: response not an image")

// Counts is the on-disk count breakdown the settings page surfaces.
// Each field is a count of files found under the matching slot kind —
// if the cache is disabled or empty, all fields are zero.
type Counts struct {
	Headshots        int `json:"headshots"`
	ShowBanners      int `json:"show_banners"`
	RecordingFanarts int `json:"recording_fanarts"`
	RecordingPosters int `json:"recording_posters"`
}

// Cache is the local image cache. Construct via New or as a struct
// literal; the zero value (Root == "") is a valid disabled cache and
// every method handles it.
type Cache struct {
	// Root is the on-disk directory. Empty disables the cache.
	Root string
	// HTTP is used for fetching upstream images. nil falls back to a
	// default with the package fetchTimeout.
	HTTP *http.Client
	// Logger is used for warn/error logs from Fetch* methods. The zero
	// value (a nop logger) is fine.
	Logger zerolog.Logger
}

// New is a convenience constructor that fills in a default HTTP client
// when one isn't supplied. Pass an empty root to construct a disabled
// cache.
func New(root string, hc *http.Client, logger zerolog.Logger) *Cache {
	if hc == nil {
		hc = &http.Client{Timeout: fetchTimeout}
	}
	return &Cache{Root: root, HTTP: hc, Logger: logger}
}

// Disabled reports whether the cache is in no-op mode (Root == "").
// Callers can short-circuit on this rather than hitting Has* / Fetch*.
func (c *Cache) Disabled() bool {
	return c == nil || c.Root == ""
}

// HeadshotPath returns the canonical filesystem path for an actor's
// headshot. Does not imply the file exists; pair with HasHeadshot.
func (c *Cache) HeadshotPath(actorID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, actorsDir, strconv.FormatInt(actorID, 10)+fileExt)
}

// ShowBannerPath returns the canonical filesystem path for a show's
// chosen banner image. Does not imply existence; pair with HasShowBanner.
func (c *Cache) ShowBannerPath(showID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, showsDir, strconv.FormatInt(showID, 10), bannerFile)
}

// RecordingFanartPath returns the canonical filesystem path for a
// recording's chosen fanart (wide, raw — no overlay).
func (c *Cache) RecordingFanartPath(recordingID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, recordingsDir,
		strconv.FormatInt(recordingID, 10), fanartFile)
}

// RecordingPosterPath returns the canonical filesystem path for a
// recording's vertical poster with burned-in overlay text.
func (c *Cache) RecordingPosterPath(recordingID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, recordingsDir,
		strconv.FormatInt(recordingID, 10), posterFile)
}

// RecordingPosterSrcPath returns the path of the raw poster source —
// the input the renderer composites the overlay onto to produce
// poster.jpg. Re-rendering reads this file rather than re-fetching.
func (c *Cache) RecordingPosterSrcPath(recordingID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, recordingsDir,
		strconv.FormatInt(recordingID, 10), posterSrcFile)
}

// HasHeadshot reports whether an actor's headshot file is on disk.
func (c *Cache) HasHeadshot(actorID int64) bool {
	return c.fileExists(c.HeadshotPath(actorID))
}

// HasShowBanner reports whether a show's banner file is on disk.
func (c *Cache) HasShowBanner(showID int64) bool {
	return c.fileExists(c.ShowBannerPath(showID))
}

// HasRecordingFanart reports whether a recording's fanart file is on disk.
func (c *Cache) HasRecordingFanart(recordingID int64) bool {
	return c.fileExists(c.RecordingFanartPath(recordingID))
}

// HasRecordingPoster reports whether a recording's overlaid poster
// file is on disk.
func (c *Cache) HasRecordingPoster(recordingID int64) bool {
	return c.fileExists(c.RecordingPosterPath(recordingID))
}

// HasRecordingPosterSrc reports whether the raw poster source is on
// disk. Used by the renderer to decide whether a re-render is possible.
func (c *Cache) HasRecordingPosterSrc(recordingID int64) bool {
	return c.fileExists(c.RecordingPosterSrcPath(recordingID))
}

// HeadshotMTime returns the on-disk modification time of the actor's
// headshot as a Unix timestamp, or 0 when the file (or cache) is
// missing. NFO writers use this to cache-bust the absolute headshot
// URL so a fresh upload propagates through media-server caches.
func (c *Cache) HeadshotMTime(actorID int64) int64 {
	return c.fileMTime(c.HeadshotPath(actorID))
}

// ShowBannerMTime returns the on-disk modification time of the show's
// banner as a Unix timestamp, or 0 when the file (or cache) is
// missing.
func (c *Cache) ShowBannerMTime(showID int64) int64 {
	return c.fileMTime(c.ShowBannerPath(showID))
}

// RecordingFanartMTime returns the on-disk modification time of the
// recording's fanart as a Unix timestamp, or 0 when the file (or
// cache) is missing.
func (c *Cache) RecordingFanartMTime(recordingID int64) int64 {
	return c.fileMTime(c.RecordingFanartPath(recordingID))
}

// RecordingPosterMTime returns the on-disk modification time of the
// recording's burned-in poster as a Unix timestamp, or 0 when the
// file (or cache) is missing.
func (c *Cache) RecordingPosterMTime(recordingID int64) int64 {
	return c.fileMTime(c.RecordingPosterPath(recordingID))
}

// HeadshotURL returns the canonical /images/... path for the actor's
// headshot. ALWAYS returns the canonical path (even when the file
// isn't on disk yet) — the server's /images/* route falls through to
// the placeholder generator on miss, so the browser gets a valid
// image either way. Empty only when the cache itself is disabled.
func (c *Cache) HeadshotURL(actorID int64) string {
	if c.Disabled() {
		return ""
	}
	return "/images/" + actorsDir + "/" + strconv.FormatInt(actorID, 10) + fileExt
}

// ShowBannerURL returns the canonical /images/... path for the show's
// banner. Same fall-through-to-placeholder semantics as HeadshotURL.
func (c *Cache) ShowBannerURL(showID int64) string {
	if c.Disabled() {
		return ""
	}
	return "/images/" + showsDir + "/" + strconv.FormatInt(showID, 10) + "/" + bannerFile
}

// RecordingFanartURL returns the canonical /images/... path for the
// recording's fanart. Same fall-through-to-placeholder semantics.
func (c *Cache) RecordingFanartURL(recordingID int64) string {
	if c.Disabled() {
		return ""
	}
	return "/images/" + recordingsDir + "/" +
		strconv.FormatInt(recordingID, 10) + "/" + fanartFile
}

// RecordingPosterURL returns the canonical /images/... path for the
// recording's burned-in poster. Same fall-through-to-placeholder
// semantics — a recording missing poster.jpg renders the generated
// placeholder card the SVG generator emits.
func (c *Cache) RecordingPosterURL(recordingID int64) string {
	if c.Disabled() {
		return ""
	}
	return "/images/" + recordingsDir + "/" +
		strconv.FormatInt(recordingID, 10) + "/" + posterFile
}

// FetchHeadshot downloads url into the canonical headshot slot.
// Idempotent: when the file already exists, the existing path is
// returned without a network call. Atomic: writes via a sibling .tmp
// file and renames into place so a partial download never replaces a
// good file.
func (c *Cache) FetchHeadshot(
	ctx context.Context, actorID int64, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.HeadshotPath(actorID), url)
}

// FetchShowBanner downloads url into the canonical banner slot.
func (c *Cache) FetchShowBanner(
	ctx context.Context, showID int64, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.ShowBannerPath(showID), url)
}

// FetchRecordingFanart downloads url into the canonical fanart slot.
func (c *Cache) FetchRecordingFanart(
	ctx context.Context, recordingID int64, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.RecordingFanartPath(recordingID), url)
}

// FetchRecordingPosterSrc downloads url into the canonical poster
// source slot. Callers typically follow up with imagerender.Regenerate
// to produce poster.jpg from the freshly-saved source.
func (c *Cache) FetchRecordingPosterSrc(
	ctx context.Context, recordingID int64, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.RecordingPosterSrcPath(recordingID), url)
}

// SaveUploadedHeadshot writes raw upload bytes to the headshot slot,
// re-encoding through image.Decode + jpeg.Encode so the on-disk shape
// is uniform regardless of upload format. The write is atomic: the
// re-encoded bytes land in a sibling .tmp file and rename into place.
func (c *Cache) SaveUploadedHeadshot(
	_ context.Context, actorID int64, body io.Reader,
) error {
	if c.Disabled() {
		return ErrDisabled
	}
	return c.writeUploadedJPEG(c.HeadshotPath(actorID), body)
}

// SaveUploadedShowBanner writes raw upload bytes to the show banner slot.
func (c *Cache) SaveUploadedShowBanner(
	_ context.Context, showID int64, body io.Reader,
) error {
	if c.Disabled() {
		return ErrDisabled
	}
	return c.writeUploadedJPEG(c.ShowBannerPath(showID), body)
}

// SaveUploadedRecordingFanart writes raw upload bytes to the fanart slot.
func (c *Cache) SaveUploadedRecordingFanart(
	_ context.Context, recordingID int64, body io.Reader,
) error {
	if c.Disabled() {
		return ErrDisabled
	}
	return c.writeUploadedJPEG(c.RecordingFanartPath(recordingID), body)
}

// SaveUploadedRecordingPosterSrc writes raw upload bytes to the poster
// source slot. Callers typically follow up with imagerender.Regenerate
// to refresh poster.jpg from the new source.
func (c *Cache) SaveUploadedRecordingPosterSrc(
	_ context.Context, recordingID int64, body io.Reader,
) error {
	if c.Disabled() {
		return ErrDisabled
	}
	return c.writeUploadedJPEG(c.RecordingPosterSrcPath(recordingID), body)
}

// Counts walks the cache root and tallies files per slot kind. Returns
// the zero Counts when disabled or when the root doesn't exist yet
// (empty cache, no failures).
func (c *Cache) Counts() Counts {
	if c.Disabled() {
		return Counts{}
	}
	out := Counts{}
	out.Headshots = c.countTree(filepath.Join(c.Root, actorsDir))
	// Show banners: count banner.jpg files under shows/<id>/.
	out.ShowBanners = c.countNamed(filepath.Join(c.Root, showsDir), bannerFile)
	// Recording fanarts + posters: count specifically-named files under
	// recordings/<id>/. poster-src.jpg isn't counted — it's an internal
	// re-render input, not a slot the user surfaces in the UI.
	out.RecordingFanarts = c.countNamed(filepath.Join(c.Root, recordingsDir), fanartFile)
	out.RecordingPosters = c.countNamed(filepath.Join(c.Root, recordingsDir), posterFile)
	return out
}

// fileExists is a small helper around os.Stat. Returns false on any
// error (including not-exist) so the file-existence-as-truth contract
// stays simple.
func (c *Cache) fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// fileMTime returns the file's mtime as a Unix timestamp, or 0 when
// the path is empty, the file is missing, or the entry is a
// directory. The 0 return is a deliberate signal: NFO writers
// distinguish "no file yet" from "file with timestamp 0" so the URL
// can be emitted without a cache-buster suffix when no image is on
// disk.
func (c *Cache) fileMTime(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.ModTime().Unix()
}

// fetchTo downloads url into dest, idempotently and atomically. The
// parent directory is created if needed. When the destination already
// exists the function returns without a network call. Errors from the
// upstream fetch are returned to the caller — they're expected to log
// and continue, never propagate up to fail a sync run.
//
// The fetched bytes are normalized to JPEG before being written: every
// cached file ends in `.jpg` and downstream consumers (the renderer,
// the browser via /images/*) expect a real JPEG. Without the
// re-encode an upstream PNG/WebP would land at `*.jpg` with non-JPEG
// magic bytes and break poster regeneration with "missing SOI marker".
func (c *Cache) fetchTo(ctx context.Context, dest, url string) (string, error) {
	if dest == "" {
		return "", ErrDisabled
	}
	if c.fileExists(dest) {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), dirMode); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}
	body, err := c.fetchBody(ctx, url)
	if err != nil {
		return "", err
	}
	jpegBytes, err := encodeAsJPEG(body)
	if err != nil {
		return "", fmt.Errorf("imagecache: %s: %w", url, err)
	}
	if writeErr := writeAtomic(dest, jpegBytes); writeErr != nil {
		return "", writeErr
	}
	c.Logger.Debug().
		Str("url", url).
		Str("dest", dest).
		Int("bytes", len(jpegBytes)).
		Msg("imagecache: stored")
	return dest, nil
}

// encodeAsJPEG decodes any image format Go's stdlib registers
// (image/jpeg, image/png, image/gif via the blank imports at the top
// of this file) and re-encodes it as a JPEG at uploadJPEGQuality.
// Used by both fetchTo and writeUploadedJPEG so cached files are
// always real JPEGs regardless of the upstream Content-Type or the
// uploader's source format. Returns an error when body isn't a
// recognizable image — bubbled up to the caller, which converts it
// to a 4xx for uploads or a soft-fail log line for fetches.
func encodeAsJPEG(body []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, img, &jpeg.Options{Quality: uploadJPEGQuality}); encErr != nil {
		return nil, fmt.Errorf("encode jpeg: %w", encErr)
	}
	return buf.Bytes(), nil
}

// writeUploadedJPEG decodes body, re-encodes the result as a JPEG at
// uploadJPEGQuality, and atomically writes it to dest. The parent
// directory is created if needed. A decode failure surfaces as an
// error (the caller maps it to HTTP 400 — bad upload) and no file is
// written.
//
// PNG transparency is flattened to whatever Go's jpeg encoder does by
// default (transparent pixels become black). Slot images are
// rectangular display assets so this is the right trade-off; a future
// opt-in could persist a parallel .png on top of the .jpg if the use
// case ever demands transparency.
func (c *Cache) writeUploadedJPEG(dest string, body io.Reader) error {
	if dest == "" {
		return ErrDisabled
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxImageBytes+1))
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	if len(raw) > maxImageBytes {
		return fmt.Errorf("upload exceeded %d bytes", maxImageBytes)
	}
	jpegBytes, err := encodeAsJPEG(raw)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(dest), dirMode); mkErr != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), mkErr)
	}
	return writeAtomic(dest, jpegBytes)
}

// writeAtomic writes body to a sibling .tmp path and renames it over
// dest on success. A partial write (interrupted by a crash) leaves the
// previous good file in place and a stray .tmp the next call's defer
// reaps.
func writeAtomic(dest string, body []byte) error {
	tmp := dest + ".tmp"
	// Best-effort cleanup of any stale tmp from a prior crash.
	if _, statErr := os.Stat(tmp); statErr == nil {
		_ = os.Remove(tmp)
	}
	if err := os.WriteFile(tmp, body, fileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if renameErr := os.Rename(tmp, dest); renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, renameErr)
	}
	return nil
}

// fetchBody issues the HTTP GET, sanity-checks the Content-Type, and
// reads the body up to maxImageBytes. The HTTP client's own timeout
// guards against slow connections; ctx adds a per-call deadline if
// the caller wants one.
func (c *Cache) fetchBody(ctx context.Context, url string) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: fetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("imagecache: %s returned %d", url, resp.StatusCode)
	}
	if !looksLikeImage(resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("%w: %s (content-type=%q)",
			ErrNotImage, url, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: %s (empty body)", ErrNotImage, url)
	}
	if len(body) > maxImageBytes {
		return nil, fmt.Errorf("imagecache: %s exceeded %d bytes", url, maxImageBytes)
	}
	return body, nil
}

// looksLikeImage returns true when the Content-Type header is an
// image/* MIME type. We don't care which subtype — JPEG, PNG, WEBP all
// flow through and we save them under .jpg regardless.
func looksLikeImage(contentType string) bool {
	if contentType == "" {
		// Empty Content-Type is suspicious but tolerated; CDNs do
		// occasionally drop the header on hot-link redirects.
		return true
	}
	return len(contentType) >= len("image/") && contentType[:len("image/")] == "image/"
}

// countTree counts regular files under dir, recursively. Missing dir
// returns 0 — an absent subdirectory just means we haven't cached
// anything in that bucket yet, not an error.
func (c *Cache) countTree(dir string) int {
	count := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		count++
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		c.Logger.Warn().Err(err).Str("dir", dir).Msg("imagecache: count walk failed")
	}
	return count
}

// countNamed walks one level of subdirectories under dir and tallies
// files matching basename. Used by Counts to count slot files (e.g.
// banner.jpg per show) without lumping in sibling files like
// poster-src.jpg.
func (c *Cache) countNamed(dir, basename string) int {
	count := 0
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		c.Logger.Warn().Err(err).Str("dir", dir).Msg("imagecache: count walk failed")
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), basename)
		if c.fileExists(path) {
			count++
		}
	}
	return count
}
