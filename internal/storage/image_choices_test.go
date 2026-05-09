package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

func TestImageChoiceFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		choice     storage.ImageChoice
		wantPoster int
		wantBack   int
		fallback   string
		wantText   string
	}{
		{
			name:       "all_unset",
			choice:     storage.ImageChoice{RecordingID: 1},
			wantPoster: 0,
			wantBack:   0,
			fallback:   "Show · 2009-12",
			wantText:   "Show · 2009-12",
		},
		{
			name: "explicit_indexes",
			choice: storage.ImageChoice{
				RecordingID:   1,
				PosterIndex:   new(2),
				BackdropIndex: new(3),
			},
			wantPoster: 2,
			wantBack:   3,
			fallback:   "fallback",
			wantText:   "fallback",
		},
		{
			name: "overlay_override",
			choice: storage.ImageChoice{
				RecordingID:         1,
				OverlayTextOverride: new("Custom Label"),
			},
			wantPoster: 0,
			wantBack:   0,
			fallback:   "fallback",
			wantText:   "Custom Label",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantPoster, tt.choice.ResolvePoster())
			assert.Equal(t, tt.wantBack, tt.choice.ResolveBackdrop())
			assert.Equal(t, tt.wantText, tt.choice.ResolveOverlayText(tt.fallback))
		})
	}
}

func TestSetAndGetImageChoice(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	const rid int64 = 90100222
	// FK constraint requires the parent recording row.
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, 1, "Test Show")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
		 VALUES (?, ?, ?, ?, ?)`,
		rid, 1, "Broadway", "2009-12-01", "{}")
	require.NoError(t, err)

	choice, err := storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Equal(t, rid, choice.RecordingID)
	assert.Nil(t, choice.PosterIndex, "fresh row should have nil poster")
	assert.Nil(t, choice.BackdropIndex)
	assert.Nil(t, choice.OverlayTextOverride)

	require.NoError(t, storage.SetPosterIndex(t.Context(), db, rid, 2))
	require.NoError(t, storage.SetBackdropIndex(t.Context(), db, rid, 1))
	require.NoError(t, storage.SetOverlayTextOverride(t.Context(), db, rid, "Greenwich Beacon · Broadway · 2017"))
	require.NoError(t, storage.SetOverlayStyle(t.Context(), db, rid, `{"color":"#cc0000"}`))

	choice, err = storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Equal(t, 2, choice.ResolvePoster())
	assert.Equal(t, 1, choice.ResolveBackdrop())
	assert.Equal(t, "Greenwich Beacon · Broadway · 2017", choice.ResolveOverlayText("fallback"))
	require.NotNil(t, choice.OverlayStyleJSON)
	assert.JSONEq(t, `{"color":"#cc0000"}`, *choice.OverlayStyleJSON)

	// Clear should null the override so fallback wins again.
	require.NoError(t, storage.ClearOverlayTextOverride(t.Context(), db, rid))
	require.NoError(t, storage.SetOverlayStyle(t.Context(), db, rid, ""))

	choice, err = storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Nil(t, choice.OverlayTextOverride)
	assert.Equal(t, "fallback", choice.ResolveOverlayText("fallback"))
	assert.Nil(t, choice.OverlayStyleJSON)
	// Index columns survive the overlay clears — upserts only touch
	// the named column.
	assert.Equal(t, 2, choice.ResolvePoster())
	assert.Equal(t, 1, choice.ResolveBackdrop())
}
