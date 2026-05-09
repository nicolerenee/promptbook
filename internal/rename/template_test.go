package rename_test

import (
	"testing"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
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

func TestApplyUnknownToken(t *testing.T) {
	t.Parallel()

	_, err := rename.Apply("{NoSuchToken}", recordingFor(t, "2024-01-21", true, true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown template token")
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
