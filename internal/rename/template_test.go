package rename_test

import (
	"testing"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/rename"
)

func recordingFor(t *testing.T, full string, monthKnown, dayKnown bool) encora.Recording {
	t.Helper()
	return encora.Recording{
		ID:     90100222,
		Show:   "Marigold Junction",
		Tour:   "Broadway",
		Master: "pro-shot",
		Date: encora.Date{
			FullDate:   full,
			MonthKnown: monthKnown,
			DayKnown:   dayKnown,
			Time:       "evening",
		},
	}
}

func TestApplyTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tmpl string
		rec  encora.Recording
		want string
	}{
		{
			name: "show_tour_id",
			tmpl: "{Show} - {Tour} [encora-{EncoraID}]",
			rec:  recordingFor(t, "2009-12-01", true, true),
			want: "Marigold Junction - Broadway [encora-90100222]",
		},
		{
			name: "smart_date_full",
			tmpl: "{Date}",
			rec:  recordingFor(t, "2024-01-21", true, true),
			want: "2024-01-21",
		},
		{
			name: "smart_date_day_unknown",
			tmpl: "{Date}",
			rec:  recordingFor(t, "2009-12-01", true, false),
			want: "December 2009",
		},
		{
			name: "smart_date_month_unknown",
			tmpl: "{Date}",
			rec:  recordingFor(t, "2009-01-01", false, false),
			want: "2009",
		},
		{
			name: "us_date_full",
			tmpl: "{DateUS}",
			rec:  recordingFor(t, "2024-01-21", true, true),
			want: "January 21, 2024",
		},
		{
			name: "numeric_date",
			tmpl: "{DateNumeric}",
			rec:  recordingFor(t, "2024-01-21", true, true),
			want: "01-21-2024",
		},
		{
			name: "year_only",
			tmpl: "{Year}",
			rec:  recordingFor(t, "2009-12-01", true, true),
			want: "2009",
		},
		{
			name: "master_token",
			tmpl: "[{Master}]",
			rec:  recordingFor(t, "2009-12-01", true, false),
			want: "[pro-shot]",
		},
		{
			name: "date_with_variant_full_no_variant",
			tmpl: "{DateWithVariant}",
			rec:  recordingFor(t, "2024-09-15", true, true),
			want: "2024-09-15",
		},
		{
			name: "date_with_variant_partial_no_variant",
			tmpl: "{DateWithVariant}",
			rec:  recordingFor(t, "2009-12-01", true, false),
			want: "2009-12",
		},
		{
			name: "date_with_variant_year_only",
			tmpl: "{DateWithVariant}",
			rec:  recordingFor(t, "1979-01-01", false, false),
			want: "1979",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rename.Apply(tt.tmpl, tt.rec)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyVariant(t *testing.T) {
	t.Parallel()

	v := "4"
	rec := recordingFor(t, "2009-12-01", true, true)
	rec.Date.DateVariant = &v

	got, err := rename.Apply("{Date} {Variant}", rec)
	require.NoError(t, err)
	assert.Equal(t, "2009-12-01 v4", got)
}

func TestApplyDateWithVariant(t *testing.T) {
	t.Parallel()

	v4 := "4"
	tests := []struct {
		name        string
		fullDate    string
		monthKnown  bool
		dayKnown    bool
		dateVariant *string
		want        string
	}{
		{
			name:        "full_with_variant",
			fullDate:    "2022-05-10",
			monthKnown:  true,
			dayKnown:    true,
			dateVariant: &v4,
			want:        "2022-05-10 (4)",
		},
		{
			name:        "month_only_with_variant",
			fullDate:    "2022-05-01",
			monthKnown:  true,
			dayKnown:    false,
			dateVariant: &v4,
			want:        "2022-05 (4)",
		},
		{
			name:        "year_only_with_variant",
			fullDate:    "1979-01-01",
			monthKnown:  false,
			dayKnown:    false,
			dateVariant: &v4,
			want:        "1979 (4)",
		},
		{
			name:        "empty_variant_renders_base",
			fullDate:    "2022-05-10",
			monthKnown:  true,
			dayKnown:    true,
			dateVariant: new(string),
			want:        "2022-05-10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := recordingFor(t, tt.fullDate, tt.monthKnown, tt.dayKnown)
			rec.Date.DateVariant = tt.dateVariant
			got, err := rename.Apply("{DateWithVariant}", rec)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyUnknownToken(t *testing.T) {
	t.Parallel()

	_, err := rename.Apply("{NoSuchToken}", recordingFor(t, "2024-01-21", true, true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown template token")
}

// TestApplyOptionalSegments covers every shape of optional-segment
// behavior the new default templates depend on: a plain ` - {Tour}`
// suffix, a bracket-wrapped `[{Master}]`, multiple references in one
// block, mixed populated/empty references that drop the whole block.
func TestApplyOptionalSegments(t *testing.T) {
	t.Parallel()

	full := encora.Recording{
		ID:     90004761,
		Show:   "Halcyon Crossing",
		Tour:   "Broadway",
		Master: "TGM",
	}
	emptyTour := full
	emptyTour.Tour = ""

	emptyMaster := full
	emptyMaster.Master = ""

	tests := []struct {
		name string
		tmpl string
		rec  encora.Recording
		want string
	}{
		{
			name: "tour_set_renders_block",
			tmpl: "{Show}{? - {Tour}}",
			rec:  full,
			want: "Halcyon Crossing - Broadway",
		},
		{
			name: "tour_empty_drops_block",
			tmpl: "{Show}{? - {Tour}}",
			rec:  emptyTour,
			want: "Halcyon Crossing",
		},
		{
			name: "master_set_brackets_block",
			tmpl: "{Show}{?[{Master}]}",
			rec:  full,
			want: "Halcyon Crossing[TGM]",
		},
		{
			name: "master_empty_brackets_drop",
			tmpl: "{Show}{?[{Master}]}",
			rec:  emptyMaster,
			want: "Halcyon Crossing",
		},
		{
			name: "multiple_optional_segments_chained",
			tmpl: "{Show}{? - {Tour}}{?[{Master}]}",
			rec:  full,
			want: "Halcyon Crossing - Broadway[TGM]",
		},
		{
			name: "all_empty_optional_segments_collapse",
			tmpl: "{Show}{? - {Tour}}{?[{Master}]}",
			rec:  encora.Recording{Show: "Sparse"},
			want: "Sparse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rename.Apply(tt.tmpl, tt.rec)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestApplyOptionalSegmentNoTokens rejects an optional segment that
// has no token references inside — there's nothing to make it
// conditional on, and the user almost certainly meant `{?{Foo}}`.
func TestApplyOptionalSegmentNoTokens(t *testing.T) {
	t.Parallel()
	_, err := rename.Apply("{Show}{? - literal-only}", encora.Recording{Show: "X"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "references no tokens")
}

// TestProbeTokensThroughBuildPlan ensures the {Container} /
// {VideoCodec} / {Quality} / {Part} tokens flow through the
// PlanInputs→ApplyContext path the way ingest exercises them.
func TestProbeTokensThroughBuildPlan(t *testing.T) {
	t.Parallel()

	rec := encora.Recording{
		ID:     90004761,
		Show:   "Halcyon Crossing",
		Tour:   "Broadway",
		Master: "TGM",
		Date: encora.Date{
			FullDate:   "2024-09-15",
			MonthKnown: true,
			DayKnown:   true,
		},
	}

	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:   rec,
		Source:      "/incoming/Halcyon Crossing.mkv",
		LibraryRoot: "/library",
		FolderTemplate: "{Show} ({DateWithVariant}) " +
			"[encora-{EncoraID}]",
		FileTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]" +
			"{? - {Tour}}{?[{Master}]}{?[{VideoCodec}]}" +
			"{?[{Quality}]}{? - part-{Part}}",
		MediaInfo: probe.MediaInfo{
			VideoCodec: "h264",
			Height:     1080,
			Width:      1920,
			Container:  "MKV",
		},
		Part: 0,
	})
	require.NoError(t, err)

	assert.Equal(t,
		"Halcyon Crossing (2024-09-15) [encora-90004761]",
		plan.TargetFolder,
	)
	assert.Equal(t,
		"Halcyon Crossing (2024-09-15) [encora-90004761] - Broadway[TGM][h264][1080p]",
		plan.TargetFile,
	)
}

// TestProbeTokensWithPart verifies the part-suffix optional segment
// only fires when Part > 0 and renders as part-1 / part-2 etc.
func TestProbeTokensWithPart(t *testing.T) {
	t.Parallel()

	rec := encora.Recording{
		ID:     90004761,
		Show:   "Halcyon Crossing",
		Tour:   "Broadway",
		Master: "TGM",
		Date: encora.Date{
			FullDate:   "2024-09-15",
			MonthKnown: true,
			DayKnown:   true,
		},
	}
	tmpl := "{Show} ({DateWithVariant}) [encora-{EncoraID}]" +
		"{? - {Tour}}{?[{Master}]}{?[{VideoCodec}]}{?[{Quality}]}" +
		"{? - part-{Part}}"

	for _, part := range []int{1, 2} {
		plan, err := rename.BuildPlan(rename.PlanInputs{
			Recording:      rec,
			Source:         "/incoming/Halcyon Crossing.mkv",
			LibraryRoot:    "/library",
			FolderTemplate: "{Show}",
			FileTemplate:   tmpl,
			MediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     1080,
				Container:  "MKV",
			},
			Part: part,
		})
		require.NoError(t, err)
		want := "Halcyon Crossing (2024-09-15) [encora-90004761] - " +
			"Broadway[TGM][h264][1080p] - part-" +
			itoa(part)
		assert.Equal(t, want, plan.TargetFile)
	}
}

// itoa keeps the test's want string assembly readable.
func itoa(n int) string {
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	default:
		return "?"
	}
}

// TestAcceptanceExamples renders the canonical examples from the
// rename redesign spec end-to-end against the new default templates,
// asserting that the optional-segment grammar + new tokens produce
// the exact strings the user signed off on.
func TestAcceptanceExamples(t *testing.T) {
	t.Parallel()

	const folderTmpl = "{Show} ({DateWithVariant}) [encora-{EncoraID}]"
	const fileTmpl = "{Show} ({DateWithVariant}) [encora-{EncoraID}]" +
		"{? - {Tour}}{?[{Master}]}{?[{VideoCodec}]}" +
		"{?[{Quality}]}{? - part-{Part}}"

	v4 := "4"

	tests := []struct {
		name         string
		rec          encora.Recording
		mediaInfo    probe.MediaInfo
		part         int
		ext          string
		wantFolder   string
		wantFileStem string
	}{
		{
			name: "1_halcyon-crossing_full_media_info",
			rec: encora.Recording{
				ID:     90004761,
				Show:   "Halcyon Crossing",
				Tour:   "Broadway",
				Master: "TGM",
				Date: encora.Date{
					FullDate:   "2024-09-15",
					MonthKnown: true,
					DayKnown:   true,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Width:      1920,
				Height:     1080,
				Container:  "MKV",
			},
			ext:          ".mkv",
			wantFolder:   "Halcyon Crossing (2024-09-15) [encora-90004761]",
			wantFileStem: "Halcyon Crossing (2024-09-15) [encora-90004761] - Broadway[TGM][h264][1080p]",
		},
		{
			name: "2_into_the_woods_with_variant",
			rec: encora.Recording{
				ID:     1125553,
				Show:   "Into the Woods",
				Tour:   "Encores!",
				Master: "pro-shot",
				Date: encora.Date{
					FullDate:    "2022-05-01",
					MonthKnown:  true,
					DayKnown:    false,
					DateVariant: &v4,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     1080,
				Container:  "MKV",
			},
			ext:        ".mkv",
			wantFolder: "Into the Woods (2022-05 (4)) [encora-1125553]",
			wantFileStem: "Into the Woods (2022-05 (4)) [encora-1125553] - " +
				"Encores![pro-shot][h264][1080p]",
		},
		{
			name: "3a_halcyon-crossing_part_1",
			rec: encora.Recording{
				ID:     90004761,
				Show:   "Halcyon Crossing",
				Tour:   "Broadway",
				Master: "TGM",
				Date: encora.Date{
					FullDate:   "2024-09-15",
					MonthKnown: true,
					DayKnown:   true,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     1080,
				Container:  "MKV",
			},
			part:       1,
			ext:        ".mkv",
			wantFolder: "Halcyon Crossing (2024-09-15) [encora-90004761]",
			wantFileStem: "Halcyon Crossing (2024-09-15) [encora-90004761] - " +
				"Broadway[TGM][h264][1080p] - part-1",
		},
		{
			name: "3b_halcyon-crossing_part_2",
			rec: encora.Recording{
				ID:     90004761,
				Show:   "Halcyon Crossing",
				Tour:   "Broadway",
				Master: "TGM",
				Date: encora.Date{
					FullDate:   "2024-09-15",
					MonthKnown: true,
					DayKnown:   true,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     1080,
				Container:  "MKV",
			},
			part:       2,
			ext:        ".mkv",
			wantFolder: "Halcyon Crossing (2024-09-15) [encora-90004761]",
			wantFileStem: "Halcyon Crossing (2024-09-15) [encora-90004761] - " +
				"Broadway[TGM][h264][1080p] - part-2",
		},
		{
			name: "4_wild_party_sparse",
			rec: encora.Recording{
				ID:   9999999,
				Show: "The Wild Party",
				Date: encora.Date{
					FullDate:   "2025-03-15",
					MonthKnown: true,
					DayKnown:   true,
				},
			},
			ext:          ".mp4",
			wantFolder:   "The Wild Party (2025-03-15) [encora-9999999]",
			wantFileStem: "The Wild Party (2025-03-15) [encora-9999999]",
		},
		{
			name: "5_marigold_partial_month_full_media",
			rec: encora.Recording{
				ID:     90100222,
				Show:   "Marigold Junction",
				Tour:   "Broadway",
				Master: "pro-shot",
				Date: encora.Date{
					FullDate:   "2009-12-01",
					MonthKnown: true,
					DayKnown:   false,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     1080,
				Container:  "MKV",
			},
			ext: ".mkv",
			// "Marigold Junction" sanitizes to "Marigold Junction".
			wantFolder: "Marigold Junction (2009-12) [encora-90100222]",
			wantFileStem: "Marigold Junction (2009-12) [encora-90100222] - " +
				"Broadway[pro-shot][h264][1080p]",
		},
		{
			name: "6_sweeney_year_only_bootleg",
			rec: encora.Recording{
				ID:     5555555,
				Show:   "Tideline Manor",
				Tour:   "Broadway",
				Master: "bootleg",
				Date: encora.Date{
					FullDate:   "1979-01-01",
					MonthKnown: false,
					DayKnown:   false,
				},
			},
			mediaInfo: probe.MediaInfo{
				VideoCodec: "h264",
				Height:     480,
				Container:  "MP4",
			},
			ext:        ".mp4",
			wantFolder: "Tideline Manor (1979) [encora-5555555]",
			wantFileStem: "Tideline Manor (1979) [encora-5555555] - " +
				"Broadway[bootleg][h264][480p]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := rename.BuildPlan(rename.PlanInputs{
				Recording:      tt.rec,
				Source:         "/incoming/file" + tt.ext,
				LibraryRoot:    "/library",
				FolderTemplate: folderTmpl,
				FileTemplate:   fileTmpl,
				MediaInfo:      tt.mediaInfo,
				Part:           tt.part,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantFolder, plan.TargetFolder)
			assert.Equal(t, tt.wantFileStem, plan.TargetFile)
		})
	}
}

// TestProbeTokensSparse covers example #4 from the spec — empty Tour,
// no master, no quality, no codec collapse cleanly via the
// optional-segment grammar.
func TestProbeTokensSparse(t *testing.T) {
	t.Parallel()

	rec := encora.Recording{
		ID:   9999999,
		Show: "The Wild Party",
		Date: encora.Date{
			FullDate:   "2025-03-15",
			MonthKnown: true,
			DayKnown:   true,
		},
	}
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      rec,
		Source:         "/incoming/wildparty.mp4",
		LibraryRoot:    "/library",
		FolderTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]",
		FileTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]" +
			"{? - {Tour}}{?[{Master}]}{?[{VideoCodec}]}" +
			"{?[{Quality}]}{? - part-{Part}}",
	})
	require.NoError(t, err)
	assert.Equal(t,
		"The Wild Party (2025-03-15) [encora-9999999]",
		plan.TargetFolder,
	)
	assert.Equal(t,
		"The Wild Party (2025-03-15) [encora-9999999]",
		plan.TargetFile,
	)
}

func TestSanitize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "passthrough",
			in:   "Show - Broadway - 2024-01-21 [encora-100]",
			want: "Show - Broadway - 2024-01-21 [encora-100]",
		},
		{name: "strip_slash", in: "Beauty/Beast", want: "BeautyBeast"},
		{name: "colon_to_dash", in: "Marigold Junction", want: "Marigold Junction"},
		{name: "collapse_spaces", in: "  Halcyon Crossing   2024  ", want: "Halcyon Crossing 2024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, rename.Sanitize(tt.in))
		})
	}
}

// Sanity check that gofakeit-driven names also survive sanitize without
// blowing up — picks up regex / encoding regressions cheaply.
func TestSanitizeFuzzed(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)

	for range 50 {
		name := gofakeit.Sentence(6)
		got := rename.Sanitize(name)
		assert.NotContains(t, got, "/")
		assert.NotContains(t, got, "\x00")
	}
}
