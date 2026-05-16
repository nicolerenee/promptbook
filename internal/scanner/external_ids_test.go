package scanner_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/scanner"
)

// TestParseExternalIDsFromName covers the bracket-style + token-spelling
// + case-insensitivity matrix the Radarr-managed folder names span.
func TestParseExternalIDsFromName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []externalids.ExternalID
	}{
		{
			name: "empty_string",
			in:   "",
			want: nil,
		},
		{
			name: "no_tags",
			in:   "Some Show (2023)",
			want: []externalids.ExternalID{},
		},
		{
			name: "square_tmdbid",
			in:   "Titanic The Musical (2023) [tmdbid-90181637]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "square_tmdb_short_token",
			in:   "Titanic The Musical (2023) [tmdb-90181637]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "curly_tmdbid",
			in:   "Titanic The Musical (2023) {tmdbid-90181637}",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "curly_tmdb_short_token",
			in:   "Titanic The Musical (2023) {tmdb-90181637}",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "square_imdb",
			in:   "Velvet Antlers Revival (2023) [imdb-tt99999999]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
			},
		},
		{
			name: "curly_imdbid",
			in:   "Velvet Antlers Revival (2023) {imdbid-tt99999999}",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
			},
		},
		{
			name: "both_tags_tmdb_then_imdb_preserves_order",
			in:   "Both (2023) [tmdbid-90181637] {imdb-tt99999999}",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
			},
		},
		{
			name: "both_tags_imdb_then_tmdb_preserves_order",
			in:   "Both (2023) [imdb-tt99999999] {tmdb-90181637}",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "case_insensitive_TMDBID",
			in:   "Caps (2023) [TMDBID-90181637]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "case_insensitive_IMDBid",
			in:   "Caps (2023) [IMDBid-tt99999999]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderIMDB, ExternalID: "tt99999999"},
			},
		},
		{
			name: "case_insensitive_mixedcase_token",
			in:   "Mixed (2023) [tMdBiD-90181637]",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
		{
			name: "imdb_must_start_with_tt",
			in:   "Bad (2023) [imdb-1234567]",
			// Without the "tt" prefix the regex doesn't match — IMDB
			// title ids always lead with lowercase tt.
			want: []externalids.ExternalID{},
		},
		{
			name: "imdb_uppercase_TT_not_matched",
			in:   "Bad (2023) [imdb-TT21435716]",
			// IMDB id capture is case-sensitive on the "tt" — uppercase
			// is invalid and skipped.
			want: []externalids.ExternalID{},
		},
		{
			name: "tmdbid_inside_parens_doesnt_match",
			// Only [...] and {...} bracket styles are recognized.
			// Parentheses are date / year delimiters.
			in:   "Bad (tmdbid-90181637)",
			want: []externalids.ExternalID{},
		},
		{
			name: "garbage_in_middle_still_finds_tag",
			in:   "Titanic - The Musical 2023 -- [tmdbid-90181637] - 1080p web-dl",
			want: []externalids.ExternalID{
				{Provider: externalids.ProviderTMDB, ExternalID: "90181637"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := scanner.ParseExternalIDsFromName(tt.in)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestScannerStampsExternalIDsOnClassification confirms that a
// folder-as-unit scan with a [tmdbid-N] folder name produces a
// classification_json blob that carries the parsed external id.
// Mirrors the existing classify_test.go fixture pattern.
func TestScannerStampsExternalIDsOnClassification(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Titanic The Musical (2023) [tmdbid-90181637]")
	main := filepath.Join(folder, "Titanic 2023.mkv")
	writeFile(t, main, 1024*1024)

	cls, _ := f.loadClassification(t)

	require.Len(t, cls.ExternalIDs, 1)
	assert.Equal(t, "tmdb", cls.ExternalIDs[0].Provider)
	assert.Equal(t, "90181637", cls.ExternalIDs[0].ExternalID)
}

// TestScannerStampsBothExternalIDsOnClassification covers the
// double-tagged folder case — someone tagged TMDB AND IMDB on the
// same folder. Both ids land on the classification.
func TestScannerStampsBothExternalIDsOnClassification(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(
		f.watchDir,
		"Velvet Antlers Revival (2023) [tmdbid-90181637] {imdb-tt99999999}",
	)
	main := filepath.Join(folder, "Velvet Antlers Revival 2023.mkv")
	writeFile(t, main, 1024*1024)

	cls, _ := f.loadClassification(t)

	require.Len(t, cls.ExternalIDs, 2)
	// On-disk order is tmdb-first; ParseExternalIDsFromName preserves
	// that.
	assert.Equal(t, "tmdb", cls.ExternalIDs[0].Provider)
	assert.Equal(t, "90181637", cls.ExternalIDs[0].ExternalID)
	assert.Equal(t, "imdb", cls.ExternalIDs[1].Provider)
	assert.Equal(t, "tt99999999", cls.ExternalIDs[1].ExternalID)
}

// TestScannerLeavesExternalIDsEmptyForUntaggedFolder confirms the
// classifier doesn't fabricate ids when nothing matches.
func TestScannerLeavesExternalIDsEmptyForUntaggedFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Plain Recording Folder")
	main := filepath.Join(folder, "Plain Recording.mkv")
	writeFile(t, main, 1024*1024)

	cls, _ := f.loadClassification(t)

	assert.Empty(t, cls.ExternalIDs)
}
