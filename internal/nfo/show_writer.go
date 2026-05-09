package nfo

// show_writer.go — emit a Plex/Jellyfin collection.nfo for a show.
//
// Plex and Jellyfin both consume a `<collection>` document at the root
// of a show's library subdirectory to surface the show as a media
// "collection" (Plex) or "boxset" (Jellyfin). The two engines disagree
// in details — Plex looks at <name>/<sorttitle>/<thumb>; Jellyfin
// reads <name>/<sorttitle>/<plot> and is happy with a sibling fanart
// element — but the union below works in both: each engine ignores
// fields it doesn't understand.
//
// `<thumb>` resolves to the show's banner image at
// shows/<show_id>/banner.jpg in the v2 image cache. The path is a
// forward-slash relative reference computed against the directory the
// NFO will land in. When no banner is cached the <thumb>/<fanart>
// elements are simply omitted.

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// CollectionNFO is the root <collection> element. Field order matches
// what Plex and Jellyfin look for when they parse the file.
type CollectionNFO struct {
	XMLName   xml.Name           `xml:"collection"`
	Name      string             `xml:"name"`
	SortTitle string             `xml:"sorttitle,omitempty"`
	Plot      string             `xml:"plot,omitempty"`
	Thumb     string             `xml:"thumb,omitempty"`
	Fanart    *CollectionFanart  `xml:"fanart,omitempty"`
	Year      string             `xml:"year,omitempty"`
	UniqueIDs []CollectionUnique `xml:"uniqueid,omitempty"`
}

// CollectionFanart wraps one or more <thumb> children inside <fanart>.
// We reuse the poster path for fanart since Encora doesn't expose
// show-level fanart art; both Plex and Jellyfin tolerate the
// duplication and either pick a real fanart later or fall back to
// the poster.
type CollectionFanart struct {
	Thumbs []string `xml:"thumb"`
}

// CollectionUnique is a `<uniqueid>` child. We surface the Encora show
// id under type="encora" so a future re-scan can de-duplicate.
type CollectionUnique struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr"`
	Value   string `xml:",chardata"`
}

// ShowWriteOptions carries the inputs WriteShowCollectionFile needs.
// Cache is optional: nil/disabled disables the <thumb>/<fanart>
// elements but still produces a valid <collection> document. DB is
// reserved for future use (audit trail) and is currently ignored.
type ShowWriteOptions struct {
	// DB is reserved for future use. Currently ignored.
	DB *sql.DB
	// Cache is the on-disk image cache. nil or Cache.Disabled() == true
	// disables poster references.
	Cache *imagecache.Cache
	// ShowID is the Encora show id. Required.
	ShowID int64
	// ShowName is the canonical show name. Used for <name> and
	// <sorttitle>; required for a useful NFO.
	ShowName string
	// Description is the Encora show description (already plain text
	// or HTML — stripHTML below normalizes it). Empty string omits
	// the <plot> element.
	Description string
	// Dir is the directory the collection.nfo lands in. The <thumb>
	// path is computed relative to it. Required.
	Dir string
	// Recordings drives the <year> derivation: the earliest parseable
	// year across this slice becomes the <year> element. Empty slice
	// produces no <year> element.
	Recordings []encora.Recording
}

// collectionNFOName is the on-disk filename Plex/Jellyfin pick up.
const collectionNFOName = "collection.nfo"

// WriteShowCollectionFile renders a `<collection>` document for the
// show and writes it to <Dir>/collection.nfo. Returns the absolute
// path written.
//
// The rendered XML is a strict superset of what either Plex or
// Jellyfin needs — both engines silently ignore unknown fields, so
// emitting more is safer than emitting less. Pass an empty cache /
// nil DB to skip the poster reference; the rest of the document is
// unaffected.
func WriteShowCollectionFile(ctx context.Context, opts ShowWriteOptions) (string, error) {
	if opts.Dir == "" {
		return "", errors.New("show nfo: empty Dir")
	}
	if opts.ShowName == "" {
		return "", fmt.Errorf("show nfo: empty ShowName for show %d", opts.ShowID)
	}
	model := buildShowNFO(ctx, opts)
	path := filepath.Join(opts.Dir, collectionNFOName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, nfoFilePerm)
	if err != nil {
		return "", fmt.Errorf("open collection nfo: %w", err)
	}
	if werr := writeCollection(f, model); werr != nil {
		_ = f.Close()
		return "", werr
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("close collection nfo: %w", cerr)
	}
	return path, nil
}

// buildShowNFO assembles the CollectionNFO model from opts. Pure
// function — no IO except the resolveShowPosterPath best-effort DB
// query (which itself swallows errors).
func buildShowNFO(ctx context.Context, opts ShowWriteOptions) CollectionNFO {
	nfo := CollectionNFO{
		Name:      opts.ShowName,
		SortTitle: opts.ShowName,
		Plot:      stripHTML(opts.Description),
		Year:      earliestYear(opts.Recordings),
	}
	if opts.ShowID > 0 {
		nfo.UniqueIDs = []CollectionUnique{{
			Type:    "encora-show",
			Default: true,
			Value:   strconv.FormatInt(opts.ShowID, 10),
		}}
	}
	if rel := resolveShowPosterPath(ctx, opts); rel != "" {
		nfo.Thumb = rel
		nfo.Fanart = &CollectionFanart{Thumbs: []string{rel}}
	}
	return nfo
}

// resolveShowPosterPath returns the NFO-relative forward-slash path to
// the show's chosen banner, or "" when imaging is disabled or the
// banner isn't on disk. Selection is implicit by file existence under
// the v2 layout — there's no per-show choice row.
func resolveShowPosterPath(_ context.Context, opts ShowWriteOptions) string {
	if opts.Cache == nil || opts.Cache.Disabled() {
		return ""
	}
	if !opts.Cache.HasShowBanner(opts.ShowID) {
		return ""
	}
	abs := opts.Cache.ShowBannerPath(opts.ShowID)
	rel, err := filepath.Rel(opts.Dir, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// earliestYear returns the smallest parseable year across the
// recordings, or "" when none parses. Mirrors yearOf in writer.go but
// works across a slice of recordings instead of a single one.
func earliestYear(recs []encora.Recording) string {
	var earliest int
	found := false
	for _, r := range recs {
		t, err := time.Parse("2006-01-02", r.Date.FullDate)
		if err != nil {
			if len(r.Date.FullDate) < isoYearLen {
				continue
			}
			var y int
			if _, scanErr := fmt.Sscanf(r.Date.FullDate[:isoYearLen], "%d", &y); scanErr != nil {
				continue
			}
			if !found || y < earliest {
				earliest = y
				found = true
			}
			continue
		}
		y := t.Year()
		if !found || y < earliest {
			earliest = y
			found = true
		}
	}
	if !found {
		return ""
	}
	return strconv.Itoa(earliest)
}

// writeCollection encodes nfo to w with the same XML header + indent
// the recording writer uses, so manual inspection of either kind of
// NFO reads consistently.
func writeCollection(w io.Writer, nfo CollectionNFO) error {
	if _, err := io.WriteString(w, xmlHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(nfo); err != nil {
		return fmt.Errorf("encode collection nfo: %w", err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("write trailing newline: %w", err)
	}
	return nil
}
