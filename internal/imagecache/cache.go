// Package imagecache stores poster, backdrop, and headshot images on
// local disk so promptbook detail pages survive upstream deletions and
// don't hot-link to third-party CDNs.
//
// The cache is file-system-backed — there is no SQLite manifest. File
// existence on disk is the only source of truth; HasX methods are
// cheap os.Stat calls. Disabled mode (Root == "") is a hard no-op:
// every method returns the empty/false zero value, callers don't have
// to nil-check the cache itself.
//
// Disk layout (canonical):
//
//	<Root>/posters/<show_id>/<index>.jpg
//	<Root>/backdrops/<recording_id>/<index>.jpg
//	<Root>/headshots/<actor_id>.jpg
//
// File extension is always .jpg. StageMedia returns JPEG and Encora's
// screen grabs are JPEG by convention; if PNG/WEBP support ever
// becomes necessary we'll add a per-source extension dance and migrate
// in place.
package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// Subdirectory names under Root. Exposed as constants so the server's
// /images/* file server and the cache writer agree on layout.
const (
	postersDir   = "posters"
	backdropsDir = "backdrops"
	headshotsDir = "headshots"

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
// Each field is a count of files found under the matching subdirectory
// — if the cache is disabled or empty, all fields are zero.
type Counts struct {
	Posters   int `json:"posters"`
	Backdrops int `json:"backdrops"`
	Headshots int `json:"headshots"`
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

// PosterPath returns the canonical filesystem path for a poster. Does
// not imply the file exists; pair with HasPoster.
func (c *Cache) PosterPath(showID int64, index int) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, postersDir, strconv.FormatInt(showID, 10),
		strconv.Itoa(index)+fileExt)
}

// BackdropPath returns the canonical filesystem path for a backdrop.
// Does not imply existence; pair with HasBackdrop.
func (c *Cache) BackdropPath(recordingID int64, index int) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, backdropsDir, strconv.FormatInt(recordingID, 10),
		strconv.Itoa(index)+fileExt)
}

// HeadshotPath returns the canonical filesystem path for a headshot.
// Does not imply existence; pair with HasHeadshot.
func (c *Cache) HeadshotPath(actorID int64) string {
	if c.Disabled() {
		return ""
	}
	return filepath.Join(c.Root, headshotsDir, strconv.FormatInt(actorID, 10)+fileExt)
}

// HasPoster reports whether a poster file is on disk. False when
// disabled or when the file is absent.
func (c *Cache) HasPoster(showID int64, index int) bool {
	return c.fileExists(c.PosterPath(showID, index))
}

// HasBackdrop reports whether a backdrop file is on disk.
func (c *Cache) HasBackdrop(recordingID int64, index int) bool {
	return c.fileExists(c.BackdropPath(recordingID, index))
}

// HasHeadshot reports whether a headshot file is on disk.
func (c *Cache) HasHeadshot(actorID int64) bool {
	return c.fileExists(c.HeadshotPath(actorID))
}

// PosterURL returns the /images/... path the server exposes for the
// cached poster, or "" when disabled or missing. Always relative —
// callers concatenate the host themselves.
func (c *Cache) PosterURL(showID int64, index int) string {
	if c.Disabled() || !c.HasPoster(showID, index) {
		return ""
	}
	return "/images/" + postersDir + "/" + strconv.FormatInt(showID, 10) +
		"/" + strconv.Itoa(index) + fileExt
}

// BackdropURL returns the /images/... path for the cached backdrop.
func (c *Cache) BackdropURL(recordingID int64, index int) string {
	if c.Disabled() || !c.HasBackdrop(recordingID, index) {
		return ""
	}
	return "/images/" + backdropsDir + "/" + strconv.FormatInt(recordingID, 10) +
		"/" + strconv.Itoa(index) + fileExt
}

// HeadshotURL returns the /images/... path for the cached headshot.
func (c *Cache) HeadshotURL(actorID int64) string {
	if c.Disabled() || !c.HasHeadshot(actorID) {
		return ""
	}
	return "/images/" + headshotsDir + "/" + strconv.FormatInt(actorID, 10) + fileExt
}

// FetchPoster downloads url into the canonical poster slot. No-op
// (returns ErrDisabled) when disabled. Returns the on-disk path on
// success. When the file already exists the existing path is returned
// without a network call — Fetch* is idempotent.
func (c *Cache) FetchPoster(
	ctx context.Context, showID int64, index int, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.PosterPath(showID, index), url)
}

// FetchBackdrop downloads url into the canonical backdrop slot.
func (c *Cache) FetchBackdrop(
	ctx context.Context, recordingID int64, index int, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.BackdropPath(recordingID, index), url)
}

// FetchHeadshot downloads url into the canonical headshot slot.
func (c *Cache) FetchHeadshot(
	ctx context.Context, actorID int64, url string,
) (string, error) {
	if c.Disabled() {
		return "", ErrDisabled
	}
	return c.fetchTo(ctx, c.HeadshotPath(actorID), url)
}

// Counts walks the cache root and tallies files per kind. Returns the
// zero Counts when disabled or when the root doesn't exist yet (empty
// cache, no failures).
func (c *Cache) Counts() Counts {
	if c.Disabled() {
		return Counts{}
	}
	return Counts{
		Posters:   c.countTree(filepath.Join(c.Root, postersDir)),
		Backdrops: c.countTree(filepath.Join(c.Root, backdropsDir)),
		Headshots: c.countTree(filepath.Join(c.Root, headshotsDir)),
	}
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

// fetchTo downloads url into dest, idempotently. The parent directory
// is created if needed. When the destination already exists the
// function returns without a network call. Errors from the upstream
// fetch are returned to the caller — they're expected to log and
// continue, never propagate up to fail a sync run.
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
	if writeErr := os.WriteFile(dest, body, fileMode); writeErr != nil {
		return "", fmt.Errorf("write %s: %w", dest, writeErr)
	}
	c.Logger.Debug().
		Str("url", url).
		Str("dest", dest).
		Int("bytes", len(body)).
		Msg("imagecache: stored")
	return dest, nil
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
