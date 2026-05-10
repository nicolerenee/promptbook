package imagerender //nolint:testpackage // tests unexported helpers.

// split_overlay_test.go — covers the row-to-slot mappings that drive
// the burned-in band layout (splitOverlay for user overrides,
// autoOverlayRows for the auto path). Lives in the package (not
// _test) so it can call both helpers directly.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
)

func TestSplitOverlay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want [overlayRowCount]string
	}{
		{
			name: "empty",
			in:   "",
			want: [overlayRowCount]string{"", "", ""},
		},
		{
			name: "all three rows present",
			in:   "2024-01-21\nFIRST US NATIONAL TOUR\nARONOFF CENTER, CINCINNATI",
			want: [overlayRowCount]string{
				"2024-01-21", "FIRST US NATIONAL TOUR", "ARONOFF CENTER, CINCINNATI",
			},
		},
		{
			name: "trailing empty row preserved (no location)",
			in:   "2023-10-26\nSECOND US NATIONAL TOUR (NON-EQUITY)\n",
			want: [overlayRowCount]string{
				"2023-10-26", "SECOND US NATIONAL TOUR (NON-EQUITY)", "",
			},
		},
		{
			name: "leading empty row preserved (no date)",
			in:   "\nBROADWAY\nWALTER KERR THEATRE",
			want: [overlayRowCount]string{
				"", "BROADWAY", "WALTER KERR THEATRE",
			},
		},
		{
			name: "middle empty row preserved (no tour)",
			in:   "2025-03-15\n\nLINCOLN CENTER",
			want: [overlayRowCount]string{
				"2025-03-15", "", "LINCOLN CENTER",
			},
		},
		{
			name: "single line lands in eyebrow slot",
			in:   "2024",
			want: [overlayRowCount]string{
				"2024", "", "",
			},
		},
		{
			name: "extra rows past 3 are dropped",
			in:   "a\nb\nc\nd\ne",
			want: [overlayRowCount]string{"a", "b", "c"},
		},
		{
			name: "whitespace per segment is trimmed",
			in:   "  2024-01-21  \n  TOUR  \n  VENUE  ",
			want: [overlayRowCount]string{"2024-01-21", "TOUR", "VENUE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitOverlay(tt.in)
			for i := range overlayRowCount {
				assert.Equal(t, tt.want[i], got[i],
					"slot %d: got %q, want %q (input %q)",
					i, got[i], tt.want[i], tt.in)
			}
		})
	}
}

func TestAutoOverlayRows(t *testing.T) {
	t.Parallel()

	mk := func(date, tour, venue, city, show string) *storage.LoadedRecording {
		return &storage.LoadedRecording{
			Recording: encora.Recording{
				Show: show,
				Tour: tour,
				Date: encora.Date{
					FullDate:   date,
					MonthKnown: len(date) >= 7,
					DayKnown:   len(date) >= 10,
				},
				Metadata: encora.RecordingMeta{
					Venue: venue,
					City:  city,
				},
			},
		}
	}

	tests := []struct {
		name string
		in   *storage.LoadedRecording
		want []string
	}{
		{
			name: "complete recording maps to all three slots",
			in: mk("2024-01-21", "FIRST US NATIONAL TOUR",
				"Aronoff Center", "Cincinnati", "Tideline Manor"),
			want: []string{
				"2024-01-21", "FIRST US NATIONAL TOUR",
				"Aronoff Center, Cincinnati",
			},
		},
		{
			name: "missing location keeps date + tour in their slots",
			in: mk("2023-10-26", "SECOND US NATIONAL TOUR (NON-EQUITY)",
				"", "", "Mockingbird Lane"),
			want: []string{
				"2023-10-26", "SECOND US NATIONAL TOUR (NON-EQUITY)", "",
			},
		},
		{
			name: "city only renders alone (no leading comma)",
			in:   mk("2024-09-22", "Broadway", "", "New York", "Halcyon Crossing"),
			want: []string{"2024-09-22", "Broadway", "New York"},
		},
		{
			name: "venue only renders alone (no trailing comma)",
			in: mk("2024-09-22", "Broadway", "Walter Kerr Theatre", "",
				"Halcyon Crossing"),
			want: []string{"2024-09-22", "Broadway", "Walter Kerr Theatre"},
		},
		{
			name: "year-only date stays year",
			in:   mk("2024-01-01", "Tour", "Venue", "City", "Show"),
			want: []string{"2024-01-01", "Tour", "Venue, City"},
		},
		{
			name: "all empty falls back to show name on title",
			in:   mk("", "", "", "", "Greenwich Beacon"),
			want: []string{"", "Greenwich Beacon", ""},
		},
		{
			name: "nil loaded yields all empty slots",
			in:   nil,
			want: []string{"", "", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := autoOverlayRows(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}
