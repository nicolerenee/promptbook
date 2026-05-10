package match_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nicolerenee/promptbook/internal/match"
)

// TestParseCorpus is the smoke battery: real filenames from the
// user's tmp/incoming folder fed through Parse() with the expected
// (show, date, tour) tuple. Anything missing or wrong here is a
// regression in the parser.
//
//nolint:gocognit // table-driven test; each case is a flat assertion.
func TestParseCorpus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantShow  string
		wantTour  string
		wantDate  match.ParsedDate
		wantSrc   string
		wantFlags map[string]bool // matinee, master, preview, act1, act2
	}{
		{
			name:     "Mockingbird Lane ISO date with bracketed source",
			in:       "Mockingbird Lane - 2022-12-22 [fixturetaper].mp4",
			wantShow: "Mockingbird Lane",
			wantDate: match.ParsedDate{Year: 2022, Month: 12, Day: 22},
			wantSrc:  "fixturetaper",
		},
		{
			name:      "Tideline Manor short ISO + matinee marker",
			in:        "Tideline Manor 2024-1-21 M.mp4",
			wantShow:  "Tideline Manor",
			wantDate:  match.ParsedDate{Year: 2024, Month: 1, Day: 21},
			wantFlags: map[string]bool{"matinee": true},
		},
		{
			name:     "Company plain ISO date",
			in:       "Company 2023-11-11.mp4",
			wantShow: "Company",
			wantDate: match.ParsedDate{Year: 2023, Month: 11, Day: 11},
		},
		{
			name:     "Velvet Antlers ISO date",
			in:       "Velvet Antlers 2023-11-29.mp4",
			wantShow: "Velvet Antlers",
			wantDate: match.ParsedDate{Year: 2023, Month: 11, Day: 29},
		},
		{
			name:      "Velvet Antlers short ISO + M",
			in:        "Velvet Antlers 2024-4-14 M.mp4",
			wantShow:  "Velvet Antlers",
			wantDate:  match.ParsedDate{Year: 2024, Month: 4, Day: 14},
			wantFlags: map[string]bool{"matinee": true},
		},
		{
			name:     "Halcyon Crossing — Broadway — Month, Year — [source]",
			in:       "Halcyon Crossing - Broadway - September, 2024 [fixturetrader14]",
			wantShow: "Halcyon Crossing",
			wantTour: "Broadway",
			wantDate: match.ParsedDate{Year: 2024, Month: 9},
			wantSrc:  "fixturetrader14",
		},
		{
			name:     "Greenwich Beacon short ISO",
			in:       "Greenwich Beacon 2024-6-7.mp4",
			wantShow: "Greenwich Beacon",
			wantDate: match.ParsedDate{Year: 2024, Month: 6, Day: 7},
		},
		{
			name:      "Lord Of The Rings short ISO + M",
			in:        "Lord Of The Rings 2024-8-28 M.mp4",
			wantShow:  "Lord Of The Rings",
			wantDate:  match.ParsedDate{Year: 2024, Month: 8, Day: 28},
			wantFlags: map[string]bool{"matinee": true},
		},
		{
			name:     "Six short ISO",
			in:       "Six 2024-6-15.mp4",
			wantShow: "Six",
			wantDate: match.ParsedDate{Year: 2024, Month: 6, Day: 15},
		},
		{
			name:     "Tideline Manor ISO + multi-token source via brackets",
			in:       "Tideline Manor - 2024-02-28 - Mateo Vance - fixturetaper.mp4",
			wantShow: "Tideline Manor",
			// "Mateo Vance" survives as a tour-ish token because it
			// trails the date; matcher will likely score this row low
			// on tour and that's OK.
			wantTour: "Mateo Vance fixturetaper",
			wantDate: match.ParsedDate{Year: 2024, Month: 2, Day: 28},
		},
		{
			name:     "Greenwich Beacon short ISO",
			in:       "Greenwich Beacon 2024-1-28.mp4",
			wantShow: "Greenwich Beacon",
			wantDate: match.ParsedDate{Year: 2024, Month: 1, Day: 28},
		},
		{
			name:     "Quill Theatre Revival with venue + Month, Year + NFT marker",
			in:       "Quill Theatre Revival - Third West End Revival - October, 2023 - NFT - fixturetaper55.mp4",
			wantShow: "Quill Theatre Revival",
			wantTour: "Third West End Revival NFT fixturetaper55",
			wantDate: match.ParsedDate{Year: 2023, Month: 10},
		},
		{
			name:     "Trillium Hall with venue + Month, Year + serial paren",
			in:       "Trillium Hall - Fifth West End Revival - July, 2022 (1) - fixturetaper55.mp4",
			wantShow: "Trillium Hall",
			wantTour: "Fifth West End Revival fixturetaper55",
			wantDate: match.ParsedDate{Year: 2022, Month: 7},
		},
		{
			name:     "Mockingbird Lane ISO date (file inside a date-named folder)",
			in:       "Mockingbird Lane 2023-10-26.mp4",
			wantShow: "Mockingbird Lane",
			wantDate: match.ParsedDate{Year: 2023, Month: 10, Day: 26},
		},
		{
			name:      "The Outsiders Month-DD-Year + Preview paren",
			in:        "The Outsiders - Mar 19 2024(Preview).mp4",
			wantShow:  "The Outsiders",
			wantDate:  match.ParsedDate{Year: 2024, Month: 3, Day: 19},
			wantFlags: map[string]bool{"preview": true},
		},
		{
			name:     "Little Shop of Horrors dotted ISO",
			in:       "Little Shop of Horrors - 03.10.2024 - pirellis miracle elixir.mp4",
			wantShow: "Little Shop of Horrors",
			wantTour: "pirellis miracle elixir",
			wantDate: match.ParsedDate{Year: 2024, Month: 3, Day: 10},
		},
		{
			name:      "Greenwich Beacon 2NT dotted ISO + matinee",
			in:        "Greenwich Beacon - 2NT - 2022.07.24 M",
			wantShow:  "Greenwich Beacon",
			wantTour:  "2NT",
			wantDate:  match.ParsedDate{Year: 2022, Month: 7, Day: 24},
			wantFlags: map[string]bool{"matinee": true},
		},
		{
			// The camelCase smush has no separator between "Notebook"
			// and "Broadway", so the parser can't tell where show
			// ends and tour begins without a known-tour vocabulary
			// (which we don't have). Result: show absorbs "Broadway"
			// and tour is empty. The matcher's fuzzy show-name pass
			// will still find "The Notebook" because Levenshtein from
			// "The Notebook Broadway" → "The Notebook" is short.
			name:     "Notebook camelCase smush — show absorbs tour",
			in:       "TheNotebookBroadwayApril2024.mp4",
			wantShow: "The Notebook Broadway",
			wantDate: match.ParsedDate{Year: 2024, Month: 4},
		},
		{
			name:     "Mockingbird Lane folder name (M-D-YYYY)",
			in:       "10-26-2023",
			wantDate: match.ParsedDate{Year: 2023, Month: 10, Day: 26},
		},
		{
			name:     "Painted Stallions short ISO inside parens",
			in:       "Painted Stallions! (US Tour Mar 2023 [5]) - mynewfavoriteday.mp4",
			wantShow: "Painted Stallions!",
			wantTour: "mynewfavoriteday",
			wantDate: match.ParsedDate{},
			wantSrc:  "5",
		},
		{
			name:     "Painted Stallions short ISO file inside its own folder",
			in:       "Painted Stallions 2024-01-06 M.mp4",
			wantShow: "Painted Stallions",
			wantDate: match.ParsedDate{Year: 2024, Month: 1, Day: 6},
			wantFlags: map[string]bool{
				"matinee": true,
			},
		},
		{
			name:     "Tideline Manor Month Year + paren serial",
			in:       "Tideline Manor - February 2024 (2).mp4",
			wantShow: "Tideline Manor",
			wantDate: match.ParsedDate{Year: 2024, Month: 2},
		},
		{
			name:     "The Quiet Almanac West End Month, Year",
			in:       "The Quiet Almanac - West End September, 2022 - fixturetrader42",
			wantShow: "The Quiet Almanac",
			wantTour: "West End fixturetrader42",
			wantDate: match.ParsedDate{Year: 2022, Month: 9},
		},
		{
			name:     "Copy of prefix + Month Year (date-only)",
			in:       "Copy of Tideline Manor - April 2022 .mp4",
			wantShow: "Copy of Tideline Manor",
			wantDate: match.ParsedDate{Year: 2022, Month: 4},
		},
		{
			name:     "HPCC abbreviation",
			in:       "HPCC 2022-12-18.mp4",
			wantShow: "HPCC",
			wantDate: match.ParsedDate{Year: 2022, Month: 12, Day: 18},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := match.Parse(tt.in)
			if tt.wantShow != "" {
				assert.Equal(t, tt.wantShow, got.ShowGuess, "show name")
			}
			if tt.wantTour != "" {
				assert.Equal(t, tt.wantTour, got.Tour, "tour")
			}
			assert.Equal(t, tt.wantDate, got.Date, "date")
			if tt.wantSrc != "" {
				assert.Equal(t, tt.wantSrc, got.Source, "source")
			}
			if tt.wantFlags["matinee"] {
				assert.True(t, got.IsMatinee, "matinee flag")
			}
			if tt.wantFlags["master"] {
				assert.True(t, got.IsMaster, "master flag")
			}
			if tt.wantFlags["preview"] {
				assert.True(t, got.IsPreview, "preview flag")
			}
			if tt.wantFlags["act1"] {
				assert.True(t, got.IsAct1, "act1 flag")
			}
			if tt.wantFlags["act2"] {
				assert.True(t, got.IsAct2, "act2 flag")
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()
	got := match.Parse("")
	assert.Equal(t, match.Parsed{}, got)
}

func TestParsedDateISO(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   match.ParsedDate
		want string
	}{
		{name: "year only pads month + day to 01",
			in: match.ParsedDate{Year: 2024}, want: "2024-01-01"},
		{name: "year + month pads day to 01",
			in: match.ParsedDate{Year: 2024, Month: 9}, want: "2024-09-01"},
		{name: "full date passes through",
			in: match.ParsedDate{Year: 2024, Month: 1, Day: 21}, want: "2024-01-21"},
		{name: "no year is empty",
			in: match.ParsedDate{Month: 9}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.in.ISO())
		})
	}
}
