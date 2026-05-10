package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/profile"
)

// ErrProfileNotSynced signals that no profile row has been written yet.
// Callers should run `promptbook collection sync` to populate it.
var ErrProfileNotSynced = errors.New("storage: profile not synced")

// Profile mirrors the cached single-row /api/profile buttonshot.
//
// LastSeenAt is preserved as the upstream string verbatim — Encora emits
// timestamps with microsecond precision and we have no need to round-trip
// them through time.Time. LastSyncedAt records when promptbook last wrote
// the row, so the UI can display a "synced N minutes ago" label.
//
// JSON tags exist because /api/v1/profile serializes this directly.
type Profile struct {
	EncoraID          int64     `json:"encora_id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	Username          string    `json:"username"`
	Status            string    `json:"status"`
	RecordingsCount   int       `json:"recordings_count"`
	WantsCount        int       `json:"wants_count"`
	LastSeenAt        string    `json:"last_seen_at"`
	ProfileVisibility string    `json:"profile_visibility"`
	ColVisibility     string    `json:"col_visibility"`
	LastSyncedAt      time.Time `json:"last_synced_at"`
}

// UpsertProfile inserts the single profile row or refreshes the existing
// one. The CHECK (id = 1) constraint on the table enforces single-row
// semantics: promptbook only ever caches the authenticated user's profile.
func UpsertProfile(ctx context.Context, client *ent.Client, p Profile) error {
	err := client.Profile.Create().
		SetID(1).
		SetEncoraID(p.EncoraID).
		SetName(p.Name).
		SetSlug(p.Slug).
		SetUsername(p.Username).
		SetStatus(p.Status).
		SetRecordingsCount(p.RecordingsCount).
		SetWantsCount(p.WantsCount).
		SetLastSeenAt(p.LastSeenAt).
		SetProfileVisibility(p.ProfileVisibility).
		SetColVisibility(p.ColVisibility).
		SetLastSyncedAt(p.LastSyncedAt).
		OnConflictColumns(profile.FieldID).
		Update(func(u *ent.ProfileUpsert) {
			u.UpdateEncoraID()
			u.UpdateName()
			u.UpdateSlug()
			u.UpdateUsername()
			u.UpdateStatus()
			u.UpdateRecordingsCount()
			u.UpdateWantsCount()
			u.UpdateLastSeenAt()
			u.UpdateProfileVisibility()
			u.UpdateColVisibility()
			u.UpdateLastSyncedAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert profile: %w", err)
	}
	return nil
}

// LoadProfile returns the cached profile row, or ErrProfileNotSynced when
// no sync has populated it yet.
func LoadProfile(ctx context.Context, client *ent.Client) (*Profile, error) {
	row, err := client.Profile.Get(ctx, 1)
	if ent.IsNotFound(err) {
		return nil, ErrProfileNotSynced
	}
	if err != nil {
		return nil, fmt.Errorf("query profile: %w", err)
	}
	return &Profile{
		EncoraID:          row.EncoraID,
		Name:              row.Name,
		Slug:              row.Slug,
		Username:          row.Username,
		Status:            row.Status,
		RecordingsCount:   row.RecordingsCount,
		WantsCount:        row.WantsCount,
		LastSeenAt:        row.LastSeenAt,
		ProfileVisibility: row.ProfileVisibility,
		ColVisibility:     row.ColVisibility,
		LastSyncedAt:      row.LastSyncedAt,
	}, nil
}
