package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

func TestUpsertAndLoadPerformer(t *testing.T) {
	t.Parallel()

	gofakeit.Seed(0)

	ctx, db := openTestDB(t)

	want := storage.Performer{
		PerformerID: gofakeit.Int64(),
		Name:        gofakeit.Name(),
		Slug:        gofakeit.LetterN(8),
		URL:         gofakeit.URL(),
		LastSeenAt:  time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, storage.UpsertPerformer(ctx, db, want))

	got, err := storage.LoadPerformer(ctx, db, want.PerformerID)
	require.NoError(t, err)
	assert.Equal(t, want.PerformerID, got.PerformerID)
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Slug, got.Slug)
	assert.Equal(t, want.URL, got.URL)
	assert.WithinDuration(t, want.LastSeenAt, got.LastSeenAt, time.Second)
}

func TestUpsertPerformerOverwrites(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	const id int64 = 90004242
	first := storage.Performer{
		PerformerID: id,
		Name:        "Delilah Sant",
		Slug:        "idina-menzel",
		URL:         "https://encora.example/p/idina-menzel",
		LastSeenAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	second := storage.Performer{
		PerformerID: id,
		Name:        "Delilah Sant (Tony winner)",
		Slug:        "idina-menzel-v2",
		URL:         "https://encora.example/p/idina-menzel-v2",
		LastSeenAt:  time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
	}

	require.NoError(t, storage.UpsertPerformer(ctx, db, first))
	require.NoError(t, storage.UpsertPerformer(ctx, db, second))

	got, err := storage.LoadPerformer(ctx, db, id)
	require.NoError(t, err)
	assert.Equal(t, second.Name, got.Name)
	assert.Equal(t, second.Slug, got.Slug)
	assert.Equal(t, second.URL, got.URL)
	assert.WithinDuration(t, second.LastSeenAt, got.LastSeenAt, time.Second)
}

func TestLoadPerformerNotFound(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	_, err := storage.LoadPerformer(ctx, db, 99999999)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrPerformerNotFound)
}

func TestUpsertAndLoadCharacter(t *testing.T) {
	t.Parallel()

	gofakeit.Seed(0)

	ctx, db := openTestDB(t)

	want := storage.Character{
		CharacterID: gofakeit.Int64(),
		Name:        gofakeit.Name(),
		Slug:        gofakeit.LetterN(8),
		URL:         gofakeit.URL(),
		LastSeenAt:  time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, storage.UpsertCharacter(ctx, db, want))

	got, err := storage.LoadCharacter(ctx, db, want.CharacterID)
	require.NoError(t, err)
	assert.Equal(t, want.CharacterID, got.CharacterID)
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Slug, got.Slug)
	assert.Equal(t, want.URL, got.URL)
	assert.WithinDuration(t, want.LastSeenAt, got.LastSeenAt, time.Second)
}

func TestLoadCharacterNotFound(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	_, err := storage.LoadCharacter(ctx, db, 99999999)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrCharacterNotFound)
}

func TestListRecordingsForPerformer(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	const (
		showID         int64 = 1
		recordingIDOne int64 = 1001
		recordingIDTwo int64 = 1002
		performerID    int64 = 7777
		characterID    int64 = 8888
	)

	seedShow(ctx, t, db, showID, "Greenwich Beacon")
	seedRecording(ctx, t, db, recordingIDOne, showID)
	seedRecording(ctx, t, db, recordingIDTwo, showID)

	// Three cast_entries: one in each recording, plus a duplicate in
	// recording one, so DISTINCT collapses to two ids.
	seedCastEntry(ctx, t, db, recordingIDOne, performerID, characterID, 0)
	seedCastEntry(ctx, t, db, recordingIDOne, performerID, characterID, 1)
	seedCastEntry(ctx, t, db, recordingIDTwo, performerID, characterID, 0)

	ids, err := storage.ListRecordingsForPerformer(ctx, db, performerID)
	require.NoError(t, err)
	assert.Equal(t, []int64{recordingIDOne, recordingIDTwo}, ids)

	// A performer with no cast_entries returns an empty (non-nil) slice.
	emptyIDs, err := storage.ListRecordingsForPerformer(ctx, db, 9999)
	require.NoError(t, err)
	require.NotNil(t, emptyIDs)
	assert.Empty(t, emptyIDs)
}

func seedCastEntry(
	ctx context.Context,
	t *testing.T,
	db *ent.Client,
	recordingID, performerID, characterID int64,
	order int,
) {
	t.Helper()
	_, err := db.CastEntry.Create().
		SetRecordingID(recordingID).
		SetPerformerID(performerID).
		SetPerformerName("").
		SetCharacterID(characterID).
		SetCharacterName("").
		SetCharacterOrder(order).
		Save(ctx)
	require.NoError(t, err)
}
