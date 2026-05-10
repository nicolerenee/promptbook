package match

// matcher.go scores parsed filenames against ent.Recording rows. The
// pipeline is deliberately heuristic: walk likely show candidates,
// score each by name similarity + date proximity + tour overlap,
// return the top N. Confidence labels (low/medium/high) wrap the
// numeric score so callers can route into the existing
// import_queue.suggested_confidence column.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
)

// Candidate is one scored match returned by Matcher.Match.
type Candidate struct {
	RecordingID int64
	ShowID      int64
	ShowName    string
	Tour        string
	DateFull    string
	Score       int
	// Reasons records the components that fed into Score so the UI
	// (or the queue's confidence column) can explain itself: e.g.
	// "show:exact (60), date:day (30), tour:overlap (10)".
	Reasons []string
}

// Confidence translates Score into the high / medium / low buckets
// the import_queue column expects. Pulled out so callers can reuse
// the thresholds without having to know the score scale.
func (c Candidate) Confidence() string {
	switch {
	case c.Score >= scoreHighThreshold:
		return ConfidenceHigh
	case c.Score >= scoreMediumThreshold:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// Confidence labels mirror storage.Confidence* — kept as locals here
// so the match package doesn't import storage (storage already
// imports ent which would create a cycle if storage depended on
// match later). They MUST stay in sync with the storage constants.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// Score component thresholds. Tuned against the tmp/incoming corpus:
//   - exact show + exact day → 130 (high)
//   - substring show + exact day → 110 (high) — covers
//     "Painted Stallions" ↔ "Painted Stallions Revival"
//   - exact show + month + tour → 110 (high) — month-only dates
//     where the tour match disambiguates between siblings
//   - exact show + month → 95 (medium)
//   - substring show alone → 50 (low) — keeps "Copy of
//     Tideline Manor" with a date that doesn't match any actual
//     Tideline Manor from getting promoted past the queue's
//     "low-confidence, prompt the user" gate
const (
	scoreHighThreshold   = 110
	scoreMediumThreshold = 60

	scoreShowExact     = 70
	scoreShowVeryClose = 55 // edit distance 1
	scoreShowClose     = 35 // edit distance 2-3
	scoreShowSubstring = 50 // one is a clean substring of the other

	scoreDateDay   = 60
	scoreDateMonth = 25
	scoreDateYear  = 5

	scoreTourMatch = 15
	// scoreMasterMatch fires when the parsed Source (bracketed
	// uploader, "[fixturetaper]") OR a tour token (a trailing
	// "- Mateo Vance") overlaps the recording's master column. The
	// master uniquely identifies a single recording when the user
	// has multiple from the same show + tour + date, so it's a
	// strong disambiguator.
	scoreMasterMatch = 25
)

// Matcher holds the dependencies the matching pipeline needs.
// Construct via New and reuse — there's no per-call state.
type Matcher struct {
	DB *ent.Client
	// MaxCandidates caps the result slice length. 5 is a friendly
	// default for the queue UI (one obvious-match + a few "did you
	// mean" alternates).
	MaxCandidates int
}

// New builds a Matcher with sensible defaults.
func New(db *ent.Client) *Matcher {
	return &Matcher{DB: db, MaxCandidates: defaultMaxCandidates}
}

const defaultMaxCandidates = 5

// Match runs the full pipeline against a parsed filename. Returns
// the top N scored Candidates in descending score order. An empty
// slice means we couldn't find anything plausible — the caller
// should leave the queue entry's suggestion fields blank.
//
//nolint:gocognit // pipeline of guard / show-loop / recording-loop / sort.
func (m *Matcher) Match(ctx context.Context, p Parsed) ([]Candidate, error) {
	if p.ShowGuess == "" {
		return nil, nil
	}

	normGuess := normalizeShowName(p.ShowGuess)
	if normGuess == "" {
		return nil, nil
	}

	shows, err := m.candidateShows(ctx, normGuess)
	if err != nil {
		return nil, fmt.Errorf("match: load candidate shows: %w", err)
	}
	if len(shows) == 0 {
		return nil, nil
	}

	candidates := make([]Candidate, 0, m.MaxCandidates*candidateExpandFactor)
	for _, s := range shows {
		showScore, showReason := scoreShow(normGuess, normalizeShowName(s.Name))
		if showScore == 0 {
			continue
		}
		recs, recErr := m.recordingsForShow(ctx, s.ID)
		if recErr != nil {
			return nil, fmt.Errorf("match: load recordings for show %d: %w", s.ID, recErr)
		}
		for _, r := range recs {
			dateScore, dateReason := scoreDate(p.Date, r.DateFull)
			tourScore, tourReason := scoreTour(p.Tour, r.Tour)
			masterScore, masterReason := scoreMaster(p, r.Master)
			total := showScore + dateScore + tourScore + masterScore
			if total <= 0 {
				continue
			}
			reasons := []string{showReason}
			if dateReason != "" {
				reasons = append(reasons, dateReason)
			}
			if tourReason != "" {
				reasons = append(reasons, tourReason)
			}
			if masterReason != "" {
				reasons = append(reasons, masterReason)
			}
			candidates = append(candidates, Candidate{
				RecordingID: r.ID,
				ShowID:      s.ID,
				ShowName:    s.Name,
				Tour:        r.Tour,
				DateFull:    r.DateFull,
				Score:       total,
				Reasons:     reasons,
			})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > m.MaxCandidates {
		candidates = candidates[:m.MaxCandidates]
	}
	return candidates, nil
}

// candidateExpandFactor sizes the initial Candidate slice generously
// so the inner show*recordings loop doesn't spend most of its time
// in slice growth.
const candidateExpandFactor = 4

// candidateShows returns the shows whose normalized name overlaps
// the normalized guess. We use a coarse contains-fold pre-filter on
// the longest meaningful word of the guess so the candidate set
// stays small without missing the right show — exact + fuzzy
// scoring happens in Match. Falls back to "all shows" when the
// pre-filter would miss everything.
func (m *Matcher) candidateShows(ctx context.Context, normGuess string) ([]*ent.Show, error) {
	tokens := strings.Fields(normGuess)
	pivot := pickPivotToken(tokens)
	q := m.DB.Show.Query()
	if pivot != "" {
		q = q.Where(show.NameContainsFold(pivot))
	}
	shows, err := q.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query shows: %w", err)
	}
	// If the pivot pre-filter excluded everything, try the un-filtered
	// set so a nickname / abbreviation still has a shot at matching.
	if len(shows) == 0 && pivot != "" {
		shows, err = m.DB.Show.Query().All(ctx)
		if err != nil {
			return nil, fmt.Errorf("query all shows fallback: %w", err)
		}
	}
	return shows, nil
}

// pickPivotToken chooses the longest non-stopword token to use as
// the contains-fold pre-filter. Length is a rough proxy for
// distinctiveness ("Notebook" matches more selectively than "the").
func pickPivotToken(tokens []string) string {
	best := ""
	for _, t := range tokens {
		if isShowStopword(t) {
			continue
		}
		if len(t) > len(best) {
			best = t
		}
	}
	return best
}

// isShowStopword filters out words that match too many shows when
// used as a pre-filter pivot. "the", "of", "and" appear in dozens
// of show titles.
func isShowStopword(t string) bool {
	switch t {
	case "the", "of", "and", "a", "an", "in", "on":
		return true
	}
	return false
}

// recordingsForShow loads every recording attached to a show. We
// load them all rather than predicate-filtering because the date
// scoring is fuzzy (day > month > year) and we want every recording
// to compete for the score, not just exact-day matches.
func (m *Matcher) recordingsForShow(ctx context.Context, showID int64) ([]*ent.Recording, error) {
	recs, err := m.DB.Recording.Query().
		Where(recording.ShowID(showID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query recordings: %w", err)
	}
	return recs, nil
}

// normalizeShowName produces a canonical form for similarity
// comparison: lowercased, punctuation stripped, multiple spaces
// collapsed to one, leading "the " dropped (so "The Notebook" ==
// "Notebook"). Must be applied to BOTH sides of any comparison.
func normalizeShowName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			// Punctuation: skip but treat as a soft separator so we
			// don't smash adjacent words together.
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimPrefix(out, "the ")
	return out
}

// scoreShow compares two normalized show-name strings. Returns the
// component score and a short reason for the candidate's reasons
// list. Zero score means "don't even bother emitting this row" — we
// won't surface a row whose show doesn't match at all.
func scoreShow(guess, candidate string) (int, string) {
	if guess == candidate {
		return scoreShowExact, "show:exact"
	}
	dist := levenshtein(guess, candidate)
	switch {
	case dist <= editDistanceTypo:
		return scoreShowVeryClose, fmt.Sprintf("show:edit-%d", dist)
	case dist <= editDistanceClose:
		return scoreShowClose, fmt.Sprintf("show:edit-%d", dist)
	}
	if strings.Contains(candidate, guess) || strings.Contains(guess, candidate) {
		return scoreShowSubstring, "show:substring"
	}
	return 0, ""
}

// scoreDate compares the parsed date against the recording's
// date_full string (always YYYY-MM-DD on disk). Day match wins; a
// month-only parsed date scores against the YYYY-MM prefix; a
// year-only date scores against the YYYY prefix.
func scoreDate(p ParsedDate, dateFull string) (int, string) {
	if !p.HasYear() || dateFull == "" {
		return 0, ""
	}
	switch {
	case p.HasDay() && dateFull == p.ISO():
		return scoreDateDay, "date:day"
	case p.HasMonth() && len(dateFull) >= isoMonthLen &&
		dateFull[:isoMonthLen] == p.ISO()[:isoMonthLen]:
		return scoreDateMonth, "date:month"
	case len(dateFull) >= isoYearLen && dateFull[:isoYearLen] == p.ISO()[:isoYearLen]:
		return scoreDateYear, "date:year"
	}
	return 0, ""
}

// scoreMaster rewards rows whose master column overlaps any of the
// noisy tokens the parser pulled out of the input — the bracketed
// Source ("[fixturetaper]") or any of the tour-token text (which
// is where Mateo Vance-style trailing names land when not in
// brackets). The master string is the most unique identifier a
// recording carries, so a hit here is a strong disambiguator
// across siblings of the same (show, tour, date).
func scoreMaster(p Parsed, recordingMaster string) (int, string) {
	if recordingMaster == "" {
		return 0, ""
	}
	master := strings.ToLower(strings.TrimSpace(recordingMaster))
	if master == "" {
		return 0, ""
	}
	for _, blob := range []string{p.Source, p.Tour} {
		if blob == "" {
			continue
		}
		if strings.Contains(strings.ToLower(blob), master) ||
			strings.Contains(master, strings.ToLower(blob)) {
			return scoreMasterMatch, "master:match"
		}
	}
	return 0, ""
}

// scoreTour rewards rows whose tour string overlaps the parsed
// tour-token set. Soft signal — we don't penalize misses, only
// boost hits, because the parser's tour extraction is the noisiest
// part of the pipeline (random uploader-name tokens leak in).
func scoreTour(parsedTour, recordingTour string) (int, string) {
	if parsedTour == "" || recordingTour == "" {
		return 0, ""
	}
	guess := normalizeShowName(parsedTour)
	candidate := normalizeShowName(recordingTour)
	if guess == "" || candidate == "" {
		return 0, ""
	}
	if strings.Contains(candidate, guess) || strings.Contains(guess, candidate) {
		return scoreTourMatch, "tour:overlap"
	}
	return 0, ""
}

// levenshtein returns the edit distance between a and b. Standard
// dynamic-programming implementation, O(len(a) * len(b)). Inputs
// must already be in their normalized form.
func levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = minInt(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func minInt(a, b, c int) int {
	return min(min(a, b), c)
}

// ISO date-string slice lengths so the date scorer doesn't sprout
// magic numbers.
const (
	isoYearLen  = 4
	isoMonthLen = 7
)

// Edit-distance thresholds for the show-name fuzzy comparator.
const (
	editDistanceTypo  = 1 // single typo or one-letter diff.
	editDistanceClose = 3 // two-to-three letter diff (e.g. "Theater" vs "Theatre").
)
