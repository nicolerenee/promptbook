package match_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/match"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestMatchCorpus exercises the full pipeline (Parse → Match) against
// the actual filenames in tmp/incoming. Each row asserts the
// recording id of the TOP candidate (or that no plausible candidate
// surfaces, for filenames whose recordings aren't in the seeded
// catalog). We use real SQLite + the production migrations rather
// than a mocked layer so the test exercises the actual ent.Recording
// / ent.Show predicates the matcher uses.
func TestMatchCorpus(t *testing.T) {
	t.Parallel()
	sqlDB, c, err := storage.OpenEnt(t.Context(),
		filepath.Join(t.TempDir(), "match.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	type rec struct {
		recID  int64
		showID int64
		name   string
		tour   string
		date   string
		master string
	}
	rows := []rec{
		{1, 100, "Mockingbird Lane", "Broadway", "2022-12-22", ""},
		{2, 101, "Tideline Manor", "First US National Tour", "2023-03-05", ""},
		{3, 101, "Tideline Manor", "First US National Tour", "2024-01-21", ""},
		{4, 102, "Drift House", "Springfield Philharmonic Concert", "2011-04-01", ""},
		{5, 102, "Drift House", "Second US National Tour", "2023-11-11", ""},
		{6, 103, "Velvet Antlers", "First US National Tour", "2023-11-29", ""},
		{7, 103, "Velvet Antlers", "First US National Tour", "2024-04-14", ""},
		{8, 104, "Halcyon Crossing", "Broadway", "2024-02-09", ""},
		{9, 104, "Halcyon Crossing", "Broadway", "2024-09-01", ""},
		{10, 105, "Harry Potter and the Cursed Child", "Broadway", "2023-12-14", ""},
		{11, 106, "Greenwich Beacon", "Riverwalk Auditorium", "2024-06-07", ""},
		{12, 107, "Mockingbird Lane", "Broadway", "2020-03-10", ""},
		{13, 107, "Mockingbird Lane", "Second US National Tour (Non-Equity)", "2023-10-26", ""},
		{14, 108, "Painted Stallions Revival", "First US National Tour", "2024-01-06", ""},
		{15, 109, "Sextet", "Second US National Tour (Coastal)", "2024-06-15", ""},
		{16, 110, "Quill Theatre Revival", "Third West End Revival", "2023-10-01", ""},
		{17, 111, "Greenwich Beacon", "Second US National Tour (Coastal)", "2024-01-28", ""},
		{18, 112, "Tideline Manor", "Broadway", "2023-03-26", ""},
		{19, 113, "Lanterns at Dawn", "Broadway", "2002-07-29", ""},
		// Twin Tideline Manor rows on the same date so the master-match
		// boost can disambiguate between them. Recording 200 has the
		// bracketed uploader as its master; recording 2 (above) is
		// the same date with no master. A filename like
		// "Tideline Manor 2023-03-05 [fixturetaper]" should pick 200.
		{200, 101, "Tideline Manor", "First US National Tour", "2023-03-05", "fixturetaper"},
	}
	uniqueShows := map[int64]string{}
	for _, r := range rows {
		uniqueShows[r.showID] = r.name
	}
	ctx := t.Context()
	for id, name := range uniqueShows {
		require.NoError(t, c.Show.Create().SetID(id).SetName(name).Exec(ctx))
	}
	for _, r := range rows {
		require.NoError(t, c.Recording.Create().
			SetID(r.recID).SetShowID(r.showID).
			SetTour(r.tour).SetDateFull(r.date).
			SetMaster(r.master).
			SetRawJSON("{}").Exec(ctx))
	}

	m := match.New(c)

	tests := []struct {
		name           string
		in             string
		wantTopRecID   int64 // 0 = expect no match
		wantConfidence string
	}{
		{name: "Mockingbird Lane exact-day", in: "Mockingbird Lane - 2022-12-22 [fixturetaper].mp4",
			wantTopRecID: 1, wantConfidence: match.ConfidenceHigh},
		{name: "Tideline Manor 2024 day", in: "Tideline Manor 2024-1-21 M.mp4",
			wantTopRecID: 3, wantConfidence: match.ConfidenceHigh},
		{
			name: "Tideline Manor with master in brackets disambiguates twins",
			in:   "Tideline Manor 2023-03-05 [fixturetaper].mp4",
			// Recordings 2 and 200 share show + tour + date; only 200
			// has master "fixturetaper". The master-match boost
			// must make 200 the top hit.
			wantTopRecID:   200,
			wantConfidence: match.ConfidenceHigh,
		},
		{name: "Drift House 2023 day", in: "Drift House 2023-11-11.mp4",
			wantTopRecID: 5, wantConfidence: match.ConfidenceHigh},
		{name: "Velvet Antlers 2023 day", in: "Velvet Antlers 2023-11-29.mp4",
			wantTopRecID: 6, wantConfidence: match.ConfidenceHigh},
		{name: "Velvet Antlers 2024 day", in: "Velvet Antlers 2024-4-14 M.mp4",
			wantTopRecID: 7, wantConfidence: match.ConfidenceHigh},
		{
			name: "Halcyon Crossing month-only with matching tour",
			in:   "Halcyon Crossing - Broadway - September, 2024 [fixturetrader14]",
			// month-only date + exact show + matching tour clears the
			// high threshold — the tour overlap disambiguates the
			// February sibling so high is correct here.
			wantTopRecID:   9,
			wantConfidence: match.ConfidenceHigh,
		},
		{name: "Greenwich Beacon Riverwalk", in: "Greenwich Beacon 2024-6-7.mp4",
			wantTopRecID: 11, wantConfidence: match.ConfidenceHigh},
		{name: "Mockingbird Lane 2023 nested folder", in: "Mockingbird Lane 2023-10-26.mp4",
			wantTopRecID: 13, wantConfidence: match.ConfidenceHigh},
		{name: "Painted Stallions file inside its folder", in: "Painted Stallions 2024-01-06 M.mp4",
			wantTopRecID: 14, wantConfidence: match.ConfidenceHigh},
		{name: "Sextet 2024 day", in: "Sextet 2024-6-15.mp4",
			wantTopRecID: 15, wantConfidence: match.ConfidenceHigh},
		{
			name:           "Quill Theatre Revival month-only",
			in:             "Quill Theatre Revival - Third West End Revival - October, 2023 - NFT - fixturetaper55.mp4",
			wantTopRecID:   16,
			wantConfidence: match.ConfidenceHigh,
		},
		{name: "Greenwich Beacon 2024 day", in: "Greenwich Beacon 2024-1-28.mp4",
			wantTopRecID: 17, wantConfidence: match.ConfidenceHigh},

		// Negative cases — recording isn't in our catalog, so the
		// top hit should still be a sibling (e.g. Tideline Manor April
		// 2022 doesn't exist; the closest by show is one of the
		// Tideline Manor tours but the date won't match). We assert
		// that the confidence drops to medium/low to keep the queue
		// from suggesting a wrong recording with high confidence.
		{name: "Copy of Tideline Manor April 2022 — no exact match",
			in: "Copy of Tideline Manor - April 2022 .mp4",
			// Tideline Manor rows are 2023-03 and 2024-01 — month-only
			// April 2022 won't match either, so just year-coarse.
			wantTopRecID: 0, wantConfidence: ""},
		{name: "HPCC abbreviation, 2022-12-18 not in catalog",
			in: "HPCC 2022-12-18.mp4",
			// HPCC won't match "Harry Potter and the Cursed Child"
			// by Levenshtein — that's a known limitation; we'd need
			// an alias system. Assert no high-confidence match.
			wantTopRecID: 0, wantConfidence: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed := match.Parse(tt.in)
			candidates, matchErr := m.Match(t.Context(), parsed)
			require.NoError(t, matchErr)
			if tt.wantTopRecID == 0 {
				// No high-confidence match expected. Either zero
				// candidates, or the top one isn't High.
				if len(candidates) > 0 {
					assert.NotEqual(t, match.ConfidenceHigh,
						candidates[0].Confidence(),
						"unexpected high-confidence top match: %+v", candidates[0])
				}
				return
			}
			require.NotEmpty(t, candidates, "no candidates")
			top := candidates[0]
			assert.Equal(t, tt.wantTopRecID, top.RecordingID,
				"unexpected top candidate: %+v", top)
			if tt.wantConfidence != "" {
				assert.Equal(t, tt.wantConfidence, top.Confidence(),
					"score=%d reasons=%v", top.Score, top.Reasons)
			}
		})
	}
}
