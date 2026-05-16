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
		name     string
		choice   storage.ImageChoice
		fallback string
		wantText string
	}{
		{
			name:     "all_unset",
			choice:   storage.ImageChoice{RecordingID: 1},
			fallback: "Show · 2009-12",
			wantText: "Show · 2009-12",
		},
		{
			name: "overlay_override",
			choice: storage.ImageChoice{
				RecordingID:         1,
				OverlayTextOverride: new("Custom Label"),
			},
			fallback: "fallback",
			wantText: "Custom Label",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantText, tt.choice.ResolveOverlayText(tt.fallback))
		})
	}
}

func TestSetAndGetImageChoice(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	const rid int64 = 90100222
	// FK constraint requires the parent recording row.
	require.NoError(t, db.Show.Create().SetID(1).SetName("Test Show").Exec(t.Context()))
	require.NoError(t, db.Recording.Create().
		SetID(rid).
		SetShowID(1).
		SetTour("Broadway").
		SetDateFull("2009-12-01").
		SetRawJSON("{}").
		Exec(t.Context()))

	choice, err := storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Equal(t, rid, choice.RecordingID)
	assert.Nil(t, choice.OverlayTextOverride)
	assert.Nil(t, choice.OverlayStyleJSON)
	assert.False(t, choice.OverlayDisabled)

	require.NoError(t, storage.SetOverlayTextOverride(t.Context(), db, rid, "Greenwich Beacon · Broadway · 2017"))
	require.NoError(t, storage.SetOverlayStyle(t.Context(), db, rid, `{"color":"#cc0000"}`))
	require.NoError(t, storage.SetOverlayDisabled(t.Context(), db, rid, true))

	choice, err = storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Equal(t, "Greenwich Beacon · Broadway · 2017", choice.ResolveOverlayText("fallback"))
	require.NotNil(t, choice.OverlayStyleJSON)
	assert.JSONEq(t, `{"color":"#cc0000"}`, *choice.OverlayStyleJSON)
	assert.True(t, choice.OverlayDisabled)

	// Clear should null the override so fallback wins again.
	require.NoError(t, storage.ClearOverlayTextOverride(t.Context(), db, rid))
	require.NoError(t, storage.SetOverlayStyle(t.Context(), db, rid, ""))
	require.NoError(t, storage.SetOverlayDisabled(t.Context(), db, rid, false))

	choice, err = storage.GetImageChoice(t.Context(), db, rid)
	require.NoError(t, err)
	assert.Nil(t, choice.OverlayTextOverride)
	assert.Equal(t, "fallback", choice.ResolveOverlayText("fallback"))
	assert.Nil(t, choice.OverlayStyleJSON)
	assert.False(t, choice.OverlayDisabled)
}
