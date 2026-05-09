package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nicolerenee/promptbook/internal/encora"
)

// ErrRecordingNotFound signals that no recording with the given id is in
// the local cache. Distinct from the encora-package error so callers can
// disambiguate "missing locally" from "missing upstream".
var ErrRecordingNotFound = errors.New("storage: recording not found")

// LoadedRecording bundles the recording payload with the local-only fields
// that are useful when printing detail (collection state, sync time).
type LoadedRecording struct {
	Recording      encora.Recording
	InCollection   bool
	InWants        bool
	Format         string
	UserNotes      *string
	UserWatched    bool
	CollectedAt    *string
	LastSyncedAt   *string
	RawJSONPresent bool
}

// LoadRecording fetches one recording from the local cache by Encora ID,
// reconstituting it from the raw_json column rather than re-assembling
// from denormalized fields.
func LoadRecording(ctx context.Context, db *sql.DB, id int64) (*LoadedRecording, error) {
	var rawJSON string
	err := db.QueryRowContext(ctx, `
		SELECT raw_json FROM recordings WHERE recording_id = ?
	`, id).Scan(&rawJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("recording %d: %w", id, ErrRecordingNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query recording: %w", err)
	}

	r, err := decodeRecording(rawJSON)
	if err != nil {
		return nil, fmt.Errorf("decode recording %d: %w", id, err)
	}
	loaded := &LoadedRecording{Recording: r, RawJSONPresent: true}

	if cerr := fillCollectionState(ctx, db, id, loaded); cerr != nil {
		return nil, cerr
	}
	if werr := fillWantsState(ctx, db, id, loaded); werr != nil {
		return nil, werr
	}
	return loaded, nil
}

func fillCollectionState(
	ctx context.Context,
	db *sql.DB,
	id int64,
	loaded *LoadedRecording,
) error {
	var (
		format       string
		userNotes    sql.NullString
		userWatched  int
		collectedAt  sql.NullString
		lastSyncedAt sql.NullString
	)
	row := db.QueryRowContext(ctx, `
		SELECT format, user_notes, user_watched, collected_at, last_synced_at
		FROM collection WHERE recording_id = ?
	`, id)
	err := row.Scan(&format, &userNotes, &userWatched, &collectedAt, &lastSyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query collection: %w", err)
	}
	loaded.InCollection = true
	loaded.Format = format
	loaded.UserWatched = userWatched != 0
	if userNotes.Valid {
		s := userNotes.String
		loaded.UserNotes = &s
	}
	if collectedAt.Valid {
		s := collectedAt.String
		loaded.CollectedAt = &s
	}
	if lastSyncedAt.Valid {
		s := lastSyncedAt.String
		loaded.LastSyncedAt = &s
	}
	return nil
}

func fillWantsState(
	ctx context.Context,
	db *sql.DB,
	id int64,
	loaded *LoadedRecording,
) error {
	var lastSynced sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT last_synced_at FROM wants WHERE recording_id = ?
	`, id).Scan(&lastSynced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query wants: %w", err)
	}
	loaded.InWants = true
	if loaded.LastSyncedAt == nil && lastSynced.Valid {
		s := lastSynced.String
		loaded.LastSyncedAt = &s
	}
	return nil
}

// ParseRecordingID validates a CLI-supplied id string.
func ParseRecordingID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid id %d", id)
	}
	return id, nil
}

func decodeRecording(rawJSON string) (encora.Recording, error) {
	var r encora.Recording
	if err := json.Unmarshal([]byte(rawJSON), &r); err != nil {
		return r, err
	}
	return r, nil
}
