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
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
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
// Jellyfin's "Collections" feature lights up automatically.
type MovieSet struct {
	Name string `xml:"name"`
}

// Actor mirrors the /library/MovieNfoSaver shape — performer name,
// character role, and a numeric order so cast renders in production order.
type Actor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
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
		Plot:          stripHTML(r.Metadata.ShowDescription),
		Set:           &MovieSet{Name: r.Show},
		Tags:          tagsOf(r),
		UniqueIDs: []UniqueID{
			{Type: "encora", Default: true, Value: strconv.FormatInt(r.ID, 10)},
		},
	}

	for _, c := range r.Cast {
		nfo.Actors = append(nfo.Actors, Actor{
			Name:  c.Performer.Name,
			Role:  c.Character.Name,
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

// renderedBackdropName is the filename the imagerender package writes
// when it produces a playbill-style burned-in backdrop. When present in
// the same backdrop directory as the raw screen-grabs, it takes
// precedence — Jellyfin/Plex pull the rendered version into their own
// metadata cache and we never want them showing the un-overlaid grab.
const renderedBackdropName = "rendered.jpg"

// WriteOptions carries the optional plumbing the writer needs to emit
// local image references in the NFO. Both DB and Cache are optional;
// when either is nil or the cache is disabled, the writer skips image
// resolution and the NFO simply omits the <thumb> / <fanart> elements.
type WriteOptions struct {
	// DB is queried for the user's image_choice row. nil disables image
	// references regardless of Cache state.
	DB *sql.DB
	// Cache is the on-disk image cache. nil or Cache.Disabled() == true
	// disables image references.
	Cache *imagecache.Cache
}

// WriteRecordingFile renders the NFO for rec into folder, including
// local poster / fanart references when opts has a usable DB + Cache.
// Returns the absolute path to the written movie.nfo.
//
// Image-reference resolution is best-effort: any error from the image
// choice lookup is swallowed silently so a transient DB hiccup never
// blocks an otherwise-successful ingest. The caller's logger picks up
// the missing references on the next manual inspection.
func WriteRecordingFile(
	ctx context.Context,
	folder string,
	rec encora.Recording,
	opts WriteOptions,
) (string, error) {
	model := FromRecording(rec)
	posterRel, fanartRel := resolveLocalImagePaths(ctx, opts.DB, opts.Cache, rec, folder)
	if posterRel != "" {
		model.Thumbs = append(model.Thumbs, Thumb{Aspect: "poster", Path: posterRel})
	}
	if fanartRel != "" {
		model.Fanart = &Fanart{Thumbs: []Thumb{{Path: fanartRel}}}
	}
	return WriteFile(folder, model)
}

// resolveLocalImagePaths returns NFO-relative paths to the poster and
// fanart images on disk, or empty strings when imaging is disabled or
// the file is missing. Both paths use forward slashes regardless of
// host OS — the Jellyfin/Plex convention. The fanart path prefers a
// rendered.jpg sibling (the imagerender output with the playbill-style
// overlay burned in) over the raw selected screen-grab.
//
// Returns (poster, fanart). Either can be empty independently.
func resolveLocalImagePaths(
	ctx context.Context,
	db *sql.DB,
	cache *imagecache.Cache,
	rec encora.Recording,
	nfoDir string,
) (string, string) {
	if cache == nil || cache.Disabled() || db == nil {
		return "", ""
	}
	choice, err := storage.GetImageChoice(ctx, db, rec.ID)
	if err != nil {
		// Best-effort: a DB read failure isn't worth failing the NFO
		// over. Caller still gets a valid NFO without image hints.
		return "", ""
	}

	return relPosterPath(cache, rec, choice, nfoDir), relFanartPath(cache, rec, choice, nfoDir)
}

// relPosterPath returns the NFO-relative path to the user's selected
// poster, or "" when the file isn't on disk.
func relPosterPath(
	cache *imagecache.Cache,
	rec encora.Recording,
	choice storage.ImageChoice,
	nfoDir string,
) string {
	idx := choice.ResolvePoster()
	if !cache.HasPoster(rec.Metadata.ShowID, idx) {
		return ""
	}
	abs := cache.PosterPath(rec.Metadata.ShowID, idx)
	return relForNFO(nfoDir, abs)
}

// relFanartPath returns the NFO-relative path to the fanart, preferring
// a rendered.jpg sibling when present. Returns "" when neither the
// rendered nor the raw selected backdrop is on disk.
func relFanartPath(
	cache *imagecache.Cache,
	rec encora.Recording,
	choice storage.ImageChoice,
	nfoDir string,
) string {
	idx := choice.ResolveBackdrop()
	rawAbs := cache.BackdropPath(rec.ID, idx)
	if rawAbs == "" {
		return ""
	}
	// rendered.jpg lives in the same directory as the raw indexed
	// backdrops. When the renderer has produced one, we point Jellyfin
	// at it instead — the playbill-style overlay is the whole point of
	// the rendered output.
	renderedAbs := filepath.Join(filepath.Dir(rawAbs), renderedBackdropName)
	if fileExists(renderedAbs) {
		return relForNFO(nfoDir, renderedAbs)
	}
	if cache.HasBackdrop(rec.ID, idx) {
		return relForNFO(nfoDir, rawAbs)
	}
	return ""
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

// fileExists is a small helper around os.Stat used by the rendered.jpg
// precedence check. Mirrors imagecache.fileExists but kept package-local
// to avoid widening that package's public surface.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
