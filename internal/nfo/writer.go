// Package nfo writes Jellyfin-flavored movie.nfo XML files alongside
// canonically-named recordings.
//
// Jellyfin's NFO scanner is forgiving — it picks up the title, plot,
// cast, year, and unique IDs. We model just enough fields for those to
// land cleanly: <title>, <year>, <premiered>, <plot>, <set>, multiple
// <actor> entries, <tag> entries, and a single <uniqueid type="encora">
// flagged as default.
package nfo

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/rename"
)

// isoYearLen is the prefix-length of year on Encora's ISO date strings.
const isoYearLen = 4

// MovieNFO is the root <movie> element. Field order maps directly to
// Jellyfin's expected NFO ordering — emit in document order so manual
// inspection reads top-to-bottom.
type MovieNFO struct {
	XMLName       xml.Name   `xml:"movie"`
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	Year          string     `xml:"year,omitempty"`
	Premiered     string     `xml:"premiered,omitempty"`
	Thumbs        []Thumb    `xml:"thumb,omitempty"`
	Fanart        *Fanart    `xml:"fanart,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	Set           *MovieSet  `xml:"set,omitempty"`
	Tags          []string   `xml:"tag,omitempty"`
	Genres        []string   `xml:"genre,omitempty"`
	Actors        []Actor    `xml:"actor,omitempty"`
	UniqueIDs     []UniqueID `xml:"uniqueid"`
	Studios       []string   `xml:"studio,omitempty"`
	FileInfo      *FileInfo  `xml:"fileinfo,omitempty"`
}

// Thumb is a single <thumb> hint that points Jellyfin/Plex at a local
// image file. The Aspect attr distinguishes poster from other thumbs;
// path is relative to the NFO's directory and uses forward slashes.
type Thumb struct {
	Aspect string `xml:"aspect,attr,omitempty"`
	Path   string `xml:",chardata"`
}

// Fanart wraps one or more <thumb> children to convey backdrop hints.
// Jellyfin expects fanart references nested under <fanart>, separate
// from the top-level poster <thumb>.
type Fanart struct {
	Thumbs []Thumb `xml:"thumb"`
}

// MovieSet groups related recordings (one show, many tours/dates) so
// Jellyfin's "Collections" feature lights up automatically. Name is
// the join key — every NFO sharing the same Name lands in the same
// collection. Overview surfaces the show description on the
// collection landing page; Thumb/Fanart point at the show banner so
// the collection has art without manual setup.
type MovieSet struct {
	Name     string     `xml:"name"`
	Overview string     `xml:"overview,omitempty"`
	Thumb    string     `xml:"thumb,omitempty"`
	Fanart   *SetFanart `xml:"fanart,omitempty"`
}

// SetFanart wraps the collection-level backdrop reference. Same shape
// as the movie-level Fanart but scoped to the parent <set> element.
type SetFanart struct {
	Thumbs []string `xml:"thumb"`
}

// Actor mirrors the /library/MovieNfoSaver shape — performer name,
// character role, optional headshot URL, and a numeric order so cast
// renders in production order. Thumb is emitted as an absolute URL
// pointing at the promptbook server's /images/actors/<id>.jpg route
// when a public URL is configured; omitted otherwise (no fallback
// exists for actor thumbs in Jellyfin/Plex).
type Actor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
	Thumb string `xml:"thumb,omitempty"`
	Order int    `xml:"order"`
}

// UniqueID lets Jellyfin de-dupe across rescans. We mark the encora id as
// default=true so Jellyfin treats it as the canonical key.
type UniqueID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr"`
	Value   string `xml:",chardata"`
}

// FileInfo is optional — Jellyfin will probe ffmpeg if it's missing —
// but emitting empty placeholders keeps the XML stable for golden tests.
type FileInfo struct {
	Notes string `xml:"notes,omitempty"`
}

// FromRecording builds a MovieNFO from an encora.Recording.
func FromRecording(r encora.Recording) MovieNFO {
	nfo := MovieNFO{
		Title:         displayTitle(r),
		OriginalTitle: r.Show,
		Year:          yearOf(r.Date),
		Premiered:     premieredOf(r.Date),
		Plot:          composePlot(r),
		Set: &MovieSet{
			Name:     r.Show,
			Overview: stripHTML(r.Metadata.ShowDescription),
		},
		Tags: tagsOf(r),
		UniqueIDs: []UniqueID{
			{Type: "encora", Default: true, Value: strconv.FormatInt(r.ID, 10)},
		},
	}

	for _, c := range r.Cast {
		nfo.Actors = append(nfo.Actors, Actor{
			Name:  c.Performer.Name,
			Role:  formatRole(c),
			Order: c.Character.Order,
		})
	}

	if r.Notes != "" || r.MasterNotes != "" {
		notes := r.Notes
		if r.MasterNotes != "" {
			if notes != "" {
				notes += "\n\n"
			}
			notes += "Master notes: " + r.MasterNotes
		}
		nfo.FileInfo = &FileInfo{Notes: notes}
	}

	return nfo
}

// composePlot builds the <plot> body from the recording's free-text
// notes + the show description. The recording's trading / general
// notes (washout, cast call-outs, intermission tidbits — the part
// specific to THIS capture) lead; the show synopsis (same across
// every recording of the show) follows after a blank line. Either
// half collapses cleanly when empty so a recording with no notes
// just gets the show description and a recording without a synced
// show description gets just the notes.
func composePlot(r encora.Recording) string {
	notes := stripHTML(r.Notes)
	desc := stripHTML(r.Metadata.ShowDescription)
	switch {
	case notes != "" && desc != "":
		return notes + "\n\n" + desc
	case notes != "":
		return notes
	default:
		return desc
	}
}

// formatRole renders the cast row's role string, prefixing the status
// abbreviation (u/s, alt, s/w, e/c, t/r) when present so understudies
// + swings + alternates surface in the Jellyfin cast list. Mirrors
// Encora's own display ("u/s Elsa") — abbreviation is passed through
// lowercase as Encora returns it; whatever capitalization the media
// server's CSS applies is up to that server.
func formatRole(c encora.CastEntry) string {
	role := c.Character.Name
	if c.Status == nil || c.Status.Abbreviation == "" {
		return role
	}
	abbrev := c.Status.Abbreviation
	if role == "" {
		return abbrev
	}
	return abbrev + " " + role
}

// displayTitle prefers a "{Show} — {Tour} — {Date}" composite so each
// recording is visually distinct from its siblings inside Jellyfin.
func displayTitle(r encora.Recording) string {
	parts := []string{r.Show}
	if r.Tour != "" {
		parts = append(parts, r.Tour)
	}
	if d := smartDate(r.Date); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, " — ")
}

// smartDate matches the rename engine's {Date} token but is reproduced
// here to avoid a circular dependency between nfo and rename packages.
func smartDate(d encora.Date) string {
	t, err := time.Parse("2006-01-02", d.FullDate)
	if err != nil {
		return d.FullDate
	}
	switch {
	case !d.MonthKnown:
		return t.Format("2006")
	case !d.DayKnown:
		return t.Format("January 2006")
	default:
		return t.Format("2006-01-02")
	}
}

func yearOf(d encora.Date) string {
	t, err := time.Parse("2006-01-02", d.FullDate)
	if err != nil {
		if len(d.FullDate) >= isoYearLen {
			return d.FullDate[:isoYearLen]
		}
		return ""
	}
	return t.Format("2006")
}

func premieredOf(d encora.Date) string {
	if !d.DayKnown {
		// Don't lie — Jellyfin parses <premiered> as a real date and
		// would otherwise pin everything to the 1st of the month.
		return ""
	}
	t, err := time.Parse("2006-01-02", d.FullDate)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// tagsOf collects boolean metadata flags into a flat tag list. Jellyfin
// surfaces these in search; saying "is_preview" is more useful than
// burying the flag inside a nested element.
func tagsOf(r encora.Recording) []string {
	var tags []string
	add := func(s string) { tags = append(tags, s) }
	if r.Metadata.RecordingType != "" {
		add(r.Metadata.RecordingType)
	}
	if r.Master != "" && r.Master != r.Metadata.RecordingType {
		add("master:" + r.Master)
	}
	if r.Metadata.IsOpening {
		add("opening night")
	}
	if r.Metadata.IsClosing {
		add("closing night")
	}
	if r.Metadata.IsPreview {
		add("preview")
	}
	if r.Metadata.IsConcert {
		add("concert")
	}
	if r.NFT.NFTForever {
		add("nft")
	}
	return tags
}

// stripHTML is intentionally minimal — Encora's show_description ships
// with `<p>` and `&#039;` entities. Jellyfin renders <plot> as plain
// text, so collapse the HTML to readable prose.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	replacers := []string{
		"<p>", "",
		"</p>", "\n\n",
		"<br>", "\n",
		"<br/>", "\n",
		"<br />", "\n",
		"&#039;", "'",
		"&quot;", `"`,
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
	}
	r := strings.NewReplacer(replacers...)
	out := strings.TrimSpace(r.Replace(s))
	return out
}

// xmlHeader is the literal first line of every NFO file.
const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
`

// Write encodes nfo to w with the standard XML header. Output is
// indented for human readability — Jellyfin doesn't care, but a stable
// indent is required for golden tests.
func Write(w io.Writer, nfo MovieNFO) error {
	if _, err := io.WriteString(w, xmlHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(nfo); err != nil {
		return fmt.Errorf("encode nfo: %w", err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("write trailing newline: %w", err)
	}
	return nil
}

// nfoFilePerm matches rename.libraryFilePerm intent — world-readable
// because Jellyfin scans these files as a different uid.
const nfoFilePerm = 0o644

// WriteFile renders nfo and writes it to {folder}/movie.nfo. Returns the
// absolute path written.
func WriteFile(folder string, nfo MovieNFO) (string, error) {
	path := filepath.Join(folder, "movie.nfo")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, nfoFilePerm)
	if err != nil {
		return "", fmt.Errorf("open nfo: %w", err)
	}
	if werr := Write(f, nfo); werr != nil {
		_ = f.Close()
		return "", werr
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("close nfo: %w", cerr)
	}
	return path, nil
}

// MovieNFOPathForPlan returns the canonical nfo path for a rename plan.
// Convenience for ingest, which wants both the rename target and the nfo
// path in scope.
func MovieNFOPathForPlan(p rename.Plan) string {
	return filepath.Join(p.AbsoluteFolder(), "movie.nfo")
}

// WriteOptions carries the optional plumbing the writer needs to emit
// image references in the NFO. Cache is optional; when nil or the
// cache is disabled, the writer skips local image resolution. DB is
// unused today — under the v2 single-file-per-slot layout image
// selection is implicit by file existence — but the field is kept
// for API stability and a future audit-trail use case.
//
// PublicURL, when non-empty, switches the writer into URL mode: the
// movie poster, fanart, and per-actor headshot elements all carry
// absolute http(s) URLs pointing at the promptbook server's /images/*
// routes so media servers (Jellyfin/Emby/Plex) can fetch the images
// over HTTP without sharing a filesystem with promptbook. When empty
// the writer falls back to today's behaviour: local sibling-file
// paths for the movie images (Cache permitting) and no <thumb> child
// on <actor> elements.
type WriteOptions struct {
	// DB is reserved for future use (audit trail of which choice was
	// in effect when the NFO was written). Currently ignored. Typed as
	// *ent.Client to match the rest of the codebase post-cutover; nil
	// is fine since the field is not consulted.
	DB *ent.Client
	// Cache is the on-disk image cache. nil or Cache.Disabled() == true
	// disables local image references.
	Cache *imagecache.Cache
	// PublicURL is the externally-reachable base URL of the promptbook
	// server (e.g. "https://promptbook.example.com"). When set, the
	// writer emits absolute /images/recordings/<id>/{poster,fanart}.jpg
	// and /images/actors/<id>.jpg URLs in place of (or alongside) the
	// local cache paths. A trailing slash is tolerated and stripped.
	PublicURL string
}

// WriteRecordingFile renders the NFO for rec into folder. When
// opts.PublicURL is set, the movie poster + fanart and every actor
// gain absolute /images/* URLs pointing at the promptbook server.
// Otherwise the writer falls back to local sibling paths via Cache
// (movie images only — actor thumbs require PublicURL because
// Jellyfin doesn't have a local-fallback convention for them).
// Returns the absolute path to the written movie.nfo.
//
// Image-reference resolution is best-effort: a missing slot file in
// local mode simply omits the corresponding element rather than
// emitting a broken reference.
//
// In URL mode each emitted image URL carries a `?v={mtime}` cache-
// buster sourced from the on-disk file's modification time (0
// suppresses the suffix entirely — bare URL — when the file isn't on
// disk yet). The companion nforefresh.Service rewrites the NFO when
// the underlying image actually changes; together they keep media-
// server caches in sync with promptbook's view of the world.
func WriteRecordingFile(
	_ context.Context,
	folder string,
	rec encora.Recording,
	opts WriteOptions,
) (string, error) {
	model := FromRecording(rec)
	if base := strings.TrimSuffix(opts.PublicURL, "/"); base != "" {
		applyPublicURLImages(&model, rec, base, opts.Cache)
	} else {
		applyLocalImages(&model, rec, folder, opts.Cache)
	}
	return WriteFile(folder, model)
}

// applyPublicURLImages mutates model to reference absolute http(s)
// URLs at the configured promptbook public URL. The /images/* layout
// is fixed by the server's image handler and kept in sync with
// imagecache's URL helpers; the writer constructs paths directly to
// avoid pulling the cache into URL mode (the cache's URL helpers
// short-circuit when Disabled, which would silently drop URLs in a
// remote-only deployment).
//
// When cache is non-nil the writer appends `?v={mtime}` to each URL
// so a freshly-uploaded image bumps the URL and media servers
// re-fetch on their next NFO scan. A missing-file mtime of 0
// suppresses the suffix; the bare URL is emitted instead so the
// wire shape stays clean for entities that don't have an image yet.
func applyPublicURLImages(
	model *MovieNFO, rec encora.Recording, base string, cache *imagecache.Cache,
) {
	posterURL := fmt.Sprintf("%s/images/recordings/%d/poster.jpg", base, rec.ID)
	fanartURL := fmt.Sprintf("%s/images/recordings/%d/fanart.jpg", base, rec.ID)
	model.Thumbs = []Thumb{{
		Aspect: "poster",
		Path:   versionedURL(posterURL, posterMTime(cache, rec.ID)),
	}}
	model.Fanart = &Fanart{Thumbs: []Thumb{{
		Path: versionedURL(fanartURL, fanartMTime(cache, rec.ID)),
	}}}
	// Collection art reuses the show banner — Jellyfin's set merge keys
	// on the set name, so every recording for a given show points at
	// the same banner URL and the collection lights up with art on
	// first import. Skipped when ShowID is missing (defensive; live
	// data always carries it).
	if model.Set != nil && rec.Metadata.ShowID > 0 {
		bannerURL := fmt.Sprintf(
			"%s/images/shows/%d/banner.jpg", base, rec.Metadata.ShowID,
		)
		bannerVersioned := versionedURL(bannerURL, bannerMTime(cache, rec.Metadata.ShowID))
		model.Set.Thumb = bannerVersioned
		model.Set.Fanart = &SetFanart{Thumbs: []string{bannerVersioned}}
	}
	// FromRecording emits one Actor per rec.Cast entry in order, so
	// indexes align 1:1 — that's how we recover each performer's id
	// for the headshot URL.
	for i := range model.Actors {
		performerID := rec.Cast[i].Performer.ID
		if performerID <= 0 {
			// Skip placeholder actors with no upstream id; without an
			// id there's no canonical headshot URL to point at.
			continue
		}
		actorURL := fmt.Sprintf("%s/images/actors/%d.jpg", base, performerID)
		model.Actors[i].Thumb = versionedURL(actorURL, headshotMTime(cache, performerID))
	}
}

// versionedURL appends a cache-buster to base. With a real on-disk
// mtime (mtime > 0) the buster is the unix-second timestamp so a
// freshly-uploaded image bumps the URL and media servers re-fetch on
// their next NFO scan. With no on-disk file (mtime == 0) the buster
// is the literal "generated" — the server's /images/* route falls
// through to the placeholder generator on cache miss, so the URL is
// still useful, and the explicit "generated" tag tells operators
// (and grep) that the URL points at a synthesized placeholder rather
// than a real upload. When the user later uploads a real image, the
// rewrite cascade replaces "generated" with the actual mtime, which
// invalidates the placeholder in any media server's URL cache.
func versionedURL(base string, mtime int64) string {
	if mtime <= 0 {
		return base + "?v=generated"
	}
	return fmt.Sprintf("%s?v=%d", base, mtime)
}

// posterMTime, fanartMTime, bannerMTime, headshotMTime are tiny
// nil-safe wrappers around the imagecache.Cache mtime helpers so
// applyPublicURLImages can stay readable. A nil cache returns 0,
// which versionedURL converts to a bare URL.
func posterMTime(cache *imagecache.Cache, recID int64) int64 {
	if cache == nil {
		return 0
	}
	return cache.RecordingPosterMTime(recID)
}

func fanartMTime(cache *imagecache.Cache, recID int64) int64 {
	if cache == nil {
		return 0
	}
	return cache.RecordingFanartMTime(recID)
}

func bannerMTime(cache *imagecache.Cache, showID int64) int64 {
	if cache == nil {
		return 0
	}
	return cache.ShowBannerMTime(showID)
}

func headshotMTime(cache *imagecache.Cache, actorID int64) int64 {
	if cache == nil {
		return 0
	}
	return cache.HeadshotMTime(actorID)
}

// applyLocalImages mutates model to reference cache-backed local
// sibling files (poster.jpg / fanart.jpg) when the cache has them on
// disk. Mirrors the pre-public-URL behaviour exactly. Actor thumbs
// are intentionally left empty — Jellyfin doesn't pick up local
// per-actor sidecar files, so emitting a relative path would just
// confuse manual inspection.
func applyLocalImages(
	model *MovieNFO, rec encora.Recording, folder string, cache *imagecache.Cache,
) {
	posterRel, fanartRel := resolveLocalImagePaths(cache, rec, folder)
	if posterRel != "" {
		model.Thumbs = append(model.Thumbs, Thumb{Aspect: "poster", Path: posterRel})
	}
	if fanartRel != "" {
		model.Fanart = &Fanart{Thumbs: []Thumb{{Path: fanartRel}}}
	}
}

// resolveLocalImagePaths returns NFO-relative paths to the poster and
// fanart images on disk, or empty strings when imaging is disabled or
// the file is missing. Both paths use forward slashes regardless of
// host OS — the Jellyfin/Plex convention.
//
// Returns (poster, fanart). Either can be empty independently. Under
// v2 the poster slot is recordings/<id>/poster.jpg (the renderer's
// burned-in output) and the fanart slot is recordings/<id>/fanart.jpg
// (the raw wide image, no overlay).
func resolveLocalImagePaths(
	cache *imagecache.Cache,
	rec encora.Recording,
	nfoDir string,
) (string, string) {
	if cache == nil || cache.Disabled() {
		return "", ""
	}

	var posterRel, fanartRel string
	if cache.HasRecordingPoster(rec.ID) {
		posterRel = relForNFO(nfoDir, cache.RecordingPosterPath(rec.ID))
	}
	if cache.HasRecordingFanart(rec.ID) {
		fanartRel = relForNFO(nfoDir, cache.RecordingFanartPath(rec.ID))
	}
	return posterRel, fanartRel
}

// relForNFO converts an absolute on-disk path into a path relative to
// nfoDir, normalized to forward slashes. Returns "" when the relative
// computation fails (different volumes on Windows, etc.) — better to
// omit the element than emit a broken reference.
func relForNFO(nfoDir, abs string) string {
	rel, err := filepath.Rel(nfoDir, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}
