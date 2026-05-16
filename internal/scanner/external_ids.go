package scanner

import (
	"regexp"

	"github.com/nicolerenee/promptbook/internal/externalids"
)

// ParseExternalIDsFromName extracts every recognized provider id
// from a folder (or file) basename. Recognized tag families:
//
//   - TMDB: [tmdbid-N], [tmdb-N], {tmdbid-N}, {tmdb-N}
//   - IMDB: [imdbid-ttN], [imdb-ttN], {imdbid-ttN}, {imdb-ttN}
//
// Tokens are matched case-insensitively (tmdbid / TMDBID / tMdB both
// hit), but the captured id retains its on-disk casing — IMDB ids
// must lead with a lowercase "tt" per Radarr's convention, so the
// regex enforces that explicitly. Multiple matches in the same name
// are returned in their on-disk order; the same id appearing twice
// returns two rows (UpsertMany absorbs the duplicate via the
// composite PK).
//
// The returned ExternalID rows leave RecordingID set to 0 — callers
// fill in the recording_id at persistence time (the scanner doesn't
// have it yet when classifying a queue row; the ingest engine
// stamps it once the recording is upserted).
//
// Pure function — no side effects, no IO. Callable from anywhere a
// folder/file name is in scope.
func ParseExternalIDsFromName(name string) []externalids.ExternalID {
	if name == "" {
		return nil
	}
	// Pre-size the slice for the most common case: at most one TMDB
	// and one IMDB id on a Radarr-managed folder name.
	const expectedMaxMatches = 2
	out := make([]externalids.ExternalID, 0, expectedMaxMatches)
	// Order matters: the regex matches return in left-to-right order
	// via FindAllStringSubmatchIndex; building each provider's hits
	// then merging by start-offset preserves on-disk order overall.
	type match struct {
		start    int
		provider externalids.Provider
		id       string
	}
	var matches []match
	for _, m := range tmdbIDRegex.FindAllStringSubmatchIndex(name, -1) {
		// m is [matchStart, matchEnd, idStart, idEnd] — the [2:] pair
		// is the id capture group.
		matches = append(matches, match{
			start:    m[0],
			provider: externalids.ProviderTMDB,
			id:       name[m[2]:m[3]],
		})
	}
	for _, m := range imdbIDRegex.FindAllStringSubmatchIndex(name, -1) {
		matches = append(matches, match{
			start:    m[0],
			provider: externalids.ProviderIMDB,
			id:       name[m[2]:m[3]],
		})
	}
	// Sort by on-disk position so multi-tag folders produce
	// deterministic output regardless of which provider's regex ran
	// first. Insertion sort is plenty for the small slice (<5 hits
	// realistic max).
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].start < matches[j-1].start; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
	for _, m := range matches {
		out = append(out, externalids.ExternalID{
			Provider:   m.provider,
			ExternalID: m.id,
		})
	}
	return out
}

// tmdbIDRegex matches the TMDB tag in either bracket style with
// either token spelling (case-insensitive). The id capture is digits
// only — Radarr's TMDB token is always numeric.
//
// Examples that match (capture group is "90181637"):
//
//	[tmdbid-90181637]   [tmdb-90181637]
//	{tmdbid-90181637}   {tmdb-90181637}
//	[TMDBID-90181637]
var tmdbIDRegex = regexp.MustCompile(`(?i)[\[\{]tmdb(?:id)?-(\d+)[\]\}]`)

// imdbIDRegex matches the IMDB tag in either bracket style with
// either token spelling. The token half is case-insensitive
// (via the `(?i)` flag scoped to the leading group); the id capture
// is case-sensitive and requires the canonical lowercase "tt"
// prefix — IMDB ids always lead with lowercase tt, so anything else
// is rejected as malformed.
//
// Examples that match (capture group is "tt99999999"):
//
//	[imdbid-tt99999999]   [imdb-tt99999999]
//	{imdbid-tt99999999}   {imdb-tt99999999}
//	[IMDBID-tt99999999]
//
// Examples that do NOT match:
//
//	[imdb-TT21435716]      (uppercase TT)
//	[imdb-1234567]         (missing tt prefix)
var imdbIDRegex = regexp.MustCompile(`(?i:[\[\{]imdb(?:id)?-)(tt\d+)(?i:[\]\}])`)
