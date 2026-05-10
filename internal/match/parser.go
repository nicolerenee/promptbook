// Package match parses recording filenames + folder names into the
// structured (show, tour, date) tuple the matcher uses to look up
// candidate ent.Recording rows.
//
// The parser is the inverse of the rename engine — rename takes a
// (recording, template) pair and produces a canonical filename; match
// takes a freeform filename written by a human and tries to recover
// the metadata so the matcher can score it against the DB. It is
// intentionally permissive: the corpus in tmp/incoming/ has fourteen
// distinct date formats, mixed CamelCase, abbreviated tour markers,
// and bracket / paren noise. The parser pulls what it can and leaves
// missing fields as zero values for the matcher to handle.
package match

import (
	"regexp"
	"strconv"
	"strings"
)

// ParsedDate is a partial-date triple. Any field can be 0 when it
// wasn't recoverable from the input. Year is the most strongly
// required field for a useful match — without it the matcher can
// only score on show-name similarity.
type ParsedDate struct {
	Year  int
	Month int
	Day   int
}

// HasYear reports whether the year field is set (non-zero).
func (d ParsedDate) HasYear() bool { return d.Year > 0 }

// HasMonth reports whether the month field is set to a valid 1-12 value.
func (d ParsedDate) HasMonth() bool { return d.Month > 0 && d.Month <= monthsPerYear }

// HasDay reports whether the day field is set to a valid 1-31 value.
func (d ParsedDate) HasDay() bool { return d.Day > 0 && d.Day <= maxDay }

// ISO returns the canonical ISO string for the date, padding unknown
// month/day fields with "01" so the resulting string can be compared
// lexicographically against ent's date_full column. Returns "" when
// the year is unknown.
func (d ParsedDate) ISO() string {
	if !d.HasYear() {
		return ""
	}
	month, day := d.Month, d.Day
	if month <= 0 {
		month = 1
	}
	if day <= 0 {
		day = 1
	}
	return iso(d.Year, month, day)
}

// Parsed is what the parser hands back. ShowGuess is the leading
// token list joined back with spaces; the matcher fuzzy-matches it
// against ent.Show.Name. Tour candidates are the run of tokens
// between the show and the date — multiple because filenames like
// "Quill Theatre Revival - Third West End Revival - October, 2023" carry
// the tour as a multi-word phrase. Source / IsMaster / IsMatinee /
// IsPreview / IsAct1 / IsAct2 are best-effort flags the matcher uses
// to sort siblings in nested folders (e.g., the user's two-act rips).
type Parsed struct {
	ShowGuess string
	Tour      string
	Date      ParsedDate
	Source    string
	IsMaster  bool
	IsMatinee bool
	IsPreview bool
	IsAct1    bool
	IsAct2    bool
}

// Parse extracts metadata from a single filename or folder name.
// The extension is stripped before parsing; pass either form. The
// caller decides whether to feed the filename, the parent folder
// name, or both (and merge results) — Parse itself is single-input.
//
// Empty / whitespace-only input returns a zero-valued Parsed.
func Parse(name string) Parsed {
	name = strings.TrimSpace(stripExt(name))
	if name == "" {
		return Parsed{}
	}

	// Insert a space before each capital letter that follows a
	// lowercase letter so smush like "TheNotebookBroadwayApril2024"
	// parses as if it had spaces. We don't apply this when there are
	// already spaces in the input (the user clearly already used
	// them) so well-formatted names aren't disturbed.
	if !strings.ContainsAny(name, " -._") {
		name = camelSplit(name)
	}

	p := Parsed{}
	name = extractBrackets(name, &p)
	name = extractParens(name, &p)
	name, p.Date = extractDate(name)
	name = applyFlags(name, &p)

	tokens := splitTokens(name)
	if len(tokens) == 0 {
		return p
	}
	p.ShowGuess = strings.TrimSpace(tokens[0])
	if len(tokens) > 1 {
		p.Tour = strings.TrimSpace(strings.Join(tokens[1:], " "))
	}
	return p
}

// extensionRE matches a "real" file extension at the end of a name:
// 1-5 alphanumeric characters preceded by a dot, anchored to end of
// string. Crucially it requires the body of the extension to be
// purely alnum — that prevents filepath.Ext-style greediness from
// chewing through dotted dates like "2022.07.24 M" (where Ext would
// hand back ".24 M").
var extensionRE = regexp.MustCompile(`\.[a-zA-Z0-9]{1,5}$`)

// stripExt removes the trailing dot-extension on a filename. Folder
// names have no extension so this is a safe no-op for them. Unlike
// filepath.Ext we DON'T treat the final dot as authoritative, since
// dotted ISO dates ("2022.07.24") would otherwise get truncated.
func stripExt(s string) string {
	loc := extensionRE.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]]
}

// camelSplit inserts a space before every uppercase letter that
// follows a lowercase letter or a digit. Used for run-on names like
// "TheNotebookBroadwayApril2024" so the date / tour extraction can
// fire normally afterward.
func camelSplit(s string) string {
	var b strings.Builder
	b.Grow(len(s) + camelSplitGrowFudge)
	prev := rune(0)
	for _, r := range s {
		if (isLower(prev) || isDigit(prev)) && isUpper(r) {
			b.WriteByte(' ')
		} else if isLetter(prev) && isDigit(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func isLower(r rune) bool  { return r >= 'a' && r <= 'z' }
func isUpper(r rune) bool  { return r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool  { return r >= '0' && r <= '9' }
func isLetter(r rune) bool { return isLower(r) || isUpper(r) }

// extractBrackets pulls every "[ ... ]" run out of name and stuffs
// the LAST one into p.Source (uploader names typically come last).
// Returns the residual string with the bracket runs removed.
var bracketsRE = regexp.MustCompile(`\[[^\[\]]*\]`)

func extractBrackets(name string, p *Parsed) string {
	all := bracketsRE.FindAllString(name, -1)
	if len(all) > 0 {
		last := all[len(all)-1]
		p.Source = strings.TrimSpace(strings.Trim(last, "[]"))
	}
	return strings.TrimSpace(bracketsRE.ReplaceAllString(name, " "))
}

// extractParens pulls every "( ... )" run out of name. Some are
// preview / act markers we promote to flags; others (a serial number
// like "(1)") are noise and silently dropped.
var parensRE = regexp.MustCompile(`\([^()]*\)`)

func extractParens(name string, p *Parsed) string {
	for _, paren := range parensRE.FindAllString(name, -1) {
		body := strings.ToLower(strings.TrimSpace(strings.Trim(paren, "()")))
		switch {
		case strings.Contains(body, "preview"):
			p.IsPreview = true
		case body == "act 1" || body == "act1" || body == "act i":
			p.IsAct1 = true
		case body == "act 2" || body == "act2" || body == "act ii":
			p.IsAct2 = true
		}
	}
	return strings.TrimSpace(parensRE.ReplaceAllString(name, " "))
}

// monthNames maps the spelled-out + 3-letter abbreviated month forms
// to their 1-12 ordinal so the date regexes don't have to repeat the
// alternation.
//
//nolint:gochecknoglobals,mnd // immutable month-ordinal lookup; numbers are inherent.
var monthNames = map[string]int{
	"january": 1, "jan": 1,
	"february": 2, "feb": 2,
	"march": 3, "mar": 3,
	"april": 4, "apr": 4,
	"may":  5,
	"june": 6, "jun": 6,
	"july": 7, "jul": 7,
	"august": 8, "aug": 8,
	"september": 9, "sept": 9, "sep": 9,
	"october": 10, "oct": 10,
	"november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

// reMonthAlt is the alternation of month-name tokens shared by the
// month-day-year and month-year regexes. Hoisted to a const so the
// long string isn't duplicated and golines doesn't complain about
// the giant inline regex.
const reMonthAlt = `jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|` +
	`jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|` +
	`nov(?:ember)?|dec(?:ember)?`

// Date regexes. Listed in order of decreasing specificity so we lock
// in the most-precise format that fits.
var (
	// 2024-01-21, 2024-1-21, 2024.01.21, 2024.1.21, 2024_01_21
	reISO = regexp.MustCompile(`\b(\d{4})[-._/](\d{1,2})[-._/](\d{1,2})\b`)
	// 03.10.2024, 10-26-2023, 1-6-2024 — month-first, 4-digit year.
	// Year-first (above) wins when both could match because reISO
	// runs first.
	reMonthFirst = regexp.MustCompile(`\b(\d{1,2})[-._/](\d{1,2})[-._/](\d{4})\b`)
	// "March 19, 2024" / "Mar 19 2024" / "April 19th, 2024".
	reMonthDayYear = regexp.MustCompile(
		`(?i)\b(` + reMonthAlt + `)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	// "April, 2024" / "April 2024" / "Mar 2023".
	reMonthYear = regexp.MustCompile(
		`(?i)\b(` + reMonthAlt + `),?\s+(\d{4})\b`)
	// Bare 4-digit year as last resort.
	reYear = regexp.MustCompile(`\b(19|20)(\d{2})\b`)
)

// extractDate scans name for the first date pattern that matches
// (most-specific format first). Returns the stripped name + the
// recovered ParsedDate. When no pattern fits the input is returned
// unchanged with a zero ParsedDate.
func extractDate(name string) (string, ParsedDate) {
	if loc := reISO.FindStringSubmatchIndex(name); loc != nil {
		match := reISO.FindStringSubmatch(name)
		y, _ := strconv.Atoi(match[1])
		mo, _ := strconv.Atoi(match[2])
		d, _ := strconv.Atoi(match[3])
		return removeRange(name, loc[0], loc[1]), ParsedDate{Year: y, Month: mo, Day: d}
	}
	if loc := reMonthFirst.FindStringSubmatchIndex(name); loc != nil {
		match := reMonthFirst.FindStringSubmatch(name)
		mo, _ := strconv.Atoi(match[1])
		d, _ := strconv.Atoi(match[2])
		y, _ := strconv.Atoi(match[3])
		return removeRange(name, loc[0], loc[1]), ParsedDate{Year: y, Month: mo, Day: d}
	}
	if loc := reMonthDayYear.FindStringSubmatchIndex(name); loc != nil {
		match := reMonthDayYear.FindStringSubmatch(name)
		mo := monthNames[strings.ToLower(match[1])]
		d, _ := strconv.Atoi(match[2])
		y, _ := strconv.Atoi(match[3])
		return removeRange(name, loc[0], loc[1]), ParsedDate{Year: y, Month: mo, Day: d}
	}
	if loc := reMonthYear.FindStringSubmatchIndex(name); loc != nil {
		match := reMonthYear.FindStringSubmatch(name)
		mo := monthNames[strings.ToLower(match[1])]
		y, _ := strconv.Atoi(match[2])
		return removeRange(name, loc[0], loc[1]), ParsedDate{Year: y, Month: mo}
	}
	if loc := reYear.FindStringSubmatchIndex(name); loc != nil {
		match := reYear.FindStringSubmatch(name)
		y, _ := strconv.Atoi(match[1] + match[2])
		return removeRange(name, loc[0], loc[1]), ParsedDate{Year: y}
	}
	return name, ParsedDate{}
}

// removeRange deletes the s[start:end] slice and collapses surrounding
// whitespace to a single space. Used by extractDate to elide the date
// match before token splitting.
func removeRange(s string, start, end int) string {
	out := s[:start] + " " + s[end:]
	return strings.Join(strings.Fields(out), " ")
}

// flagWords is the set of single-word markers we promote to bool
// flags rather than letting them leak into the ShowGuess. These are
// case-insensitive.
var flagWords = map[string]func(*Parsed){ //nolint:gochecknoglobals // immutable lookup
	"m":        func(p *Parsed) { p.IsMatinee = true },
	"matinee":  func(p *Parsed) { p.IsMatinee = true },
	"master":   func(p *Parsed) { p.IsMaster = true },
	"preview":  func(p *Parsed) { p.IsPreview = true },
	"previews": func(p *Parsed) { p.IsPreview = true },
	"act":      nil, // handled by the act-with-number sniffer below.
}

// applyFlags walks the trailing tokens of name and consumes any that
// are single-letter / single-word performance markers, setting the
// matching flag. "Trailing" because in practice these always appear
// after the date ("... 2024-1-21 M"). Stops at the first non-flag
// token so we don't eat into the show name.
func applyFlags(name string, p *Parsed) string {
	tokens := strings.Fields(name)
	if len(tokens) == 0 {
		return name
	}
	for len(tokens) > 0 {
		last := strings.ToLower(strings.Trim(tokens[len(tokens)-1], ".,"))
		if last == "act" && len(tokens) >= 2 {
			penultimate := strings.ToLower(strings.Trim(tokens[len(tokens)-2], ".,"))
			_ = penultimate
		}
		fn, ok := flagWords[last]
		if !ok {
			break
		}
		if fn != nil {
			fn(p)
		}
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, " ")
}

// splitTokens splits name into tour tokens. Treats " - " (with
// surrounding whitespace) as a strong separator and a single "-" as
// a weak one (we don't split on weak -, since show names like
// "Lord-of-the-Rings" would shatter — though no such name appears in
// the corpus today, we lean conservative). Empty / whitespace tokens
// are dropped, and trailing/leading dash runs (left over after
// extractDate ate the middle of "Show - DATE - Tour") are trimmed.
func splitTokens(name string) []string {
	parts := strings.Split(name, " - ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Inside a tour token a comma sometimes separates an extra
		// modifier ("West End, September, 2022") — but the date is
		// already extracted, so what's left is just garbage commas.
		// Trim them. Also trim orphan dashes left behind when the
		// date sat between two " - " separators (the inner pair
		// collapsed but the outer dashes survived as a string-edge
		// artifact like "Show - " or " - Tour").
		p = strings.Trim(p, " -,")
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// iso formats year/month/day as YYYY-MM-DD. Pulled out so the
// callers don't have to import fmt for the one-line format.
func iso(y, m, d int) string {
	return strconv.Itoa(y) + "-" + zeroPad(m) + "-" + zeroPad(d)
}

func zeroPad(n int) string {
	if n < dateZeroPadCutoff {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Numeric constants pulled out so mnd lint stays happy.
const (
	monthsPerYear       = 12
	maxDay              = 31
	dateZeroPadCutoff   = 10
	camelSplitGrowFudge = 8
)
