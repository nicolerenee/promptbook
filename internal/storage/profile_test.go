package storage_test

import (
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

func TestUpsertAndLoadProfile(t *testing.T) {
	t.Parallel()

	gofakeit.Seed(0)

	ctx, db := openTestDB(t)

	want := storage.Profile{
		EncoraID:          gofakeit.Int64(),
		Name:              gofakeit.Username(),
		Slug:              gofakeit.LetterN(8),
		Username:          gofakeit.Username(),
		Status:            "open",
		RecordingsCount:   28,
		WantsCount:        14,
		LastSeenAt:        "2026-05-09T00:00:05.000000Z",
		ProfileVisibility: "public",
		ColVisibility:     "public",
		LastSyncedAt:      time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, storage.UpsertProfile(ctx, db, want))

	got, err := storage.LoadProfile(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, want.EncoraID, got.EncoraID)
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Slug, got.Slug)
	assert.Equal(t, want.Username, got.Username)
	assert.Equal(t, want.Status, got.Status)
	assert.Equal(t, want.RecordingsCount, got.RecordingsCount)
	assert.Equal(t, want.WantsCount, got.WantsCount)
	assert.Equal(t, want.LastSeenAt, got.LastSeenAt)
	assert.Equal(t, want.ProfileVisibility, got.ProfileVisibility)
	assert.Equal(t, want.ColVisibility, got.ColVisibility)
	assert.WithinDuration(t, want.LastSyncedAt, got.LastSyncedAt, time.Second)
}

func TestUpsertProfileOverwrites(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	first := storage.Profile{
		EncoraID:        90007787,
		Username:        "fixturearchive",
		RecordingsCount: 28,
		WantsCount:      14,
		LastSyncedAt:    time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	second := storage.Profile{
		EncoraID:        90007787,
		Username:        "fixturearchive",
		RecordingsCount: 30, // bumped after another sync
		WantsCount:      16,
		LastSyncedAt:    time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
	}

	require.NoError(t, storage.UpsertProfile(ctx, db, first))
	require.NoError(t, storage.UpsertProfile(ctx, db, second))

	got, err := storage.LoadProfile(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, second.RecordingsCount, got.RecordingsCount)
	assert.Equal(t, second.WantsCount, got.WantsCount)
	assert.WithinDuration(t, second.LastSyncedAt, got.LastSyncedAt, time.Second)

	// CHECK (id = 1) means no second row ever lands.
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profile`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestLoadProfileNotSynced(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	_, err := storage.LoadProfile(ctx, db)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrProfileNotSynced)
}
