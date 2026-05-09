package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
func UpsertProfile(ctx context.Context, db *sql.DB, p Profile) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO profile (
			id, encora_id, name, slug, username, status,
			recordings_count, wants_count,
			last_seen_at, profile_visibility, col_visibility,
			last_synced_at
		) VALUES (
			1, ?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?,
			?
		)
		ON CONFLICT(id) DO UPDATE SET
			encora_id          = excluded.encora_id,
			name               = excluded.name,
			slug               = excluded.slug,
			username           = excluded.username,
			status             = excluded.status,
			recordings_count   = excluded.recordings_count,
			wants_count        = excluded.wants_count,
			last_seen_at       = excluded.last_seen_at,
			profile_visibility = excluded.profile_visibility,
			col_visibility     = excluded.col_visibility,
			last_synced_at     = excluded.last_synced_at
	`,
		p.EncoraID, p.Name, p.Slug, p.Username, p.Status,
		p.RecordingsCount, p.WantsCount,
		p.LastSeenAt, p.ProfileVisibility, p.ColVisibility,
		p.LastSyncedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert profile: %w", err)
	}
	return nil
}

// LoadProfile returns the cached profile row, or ErrProfileNotSynced when
// no sync has populated it yet.
func LoadProfile(ctx context.Context, db *sql.DB) (*Profile, error) {
	var p Profile
	err := db.QueryRowContext(ctx, `
		SELECT encora_id, name, slug, username, status,
		       recordings_count, wants_count,
		       last_seen_at, profile_visibility, col_visibility,
		       last_synced_at
		FROM profile
		WHERE id = 1
	`).Scan(
		&p.EncoraID, &p.Name, &p.Slug, &p.Username, &p.Status,
		&p.RecordingsCount, &p.WantsCount,
		&p.LastSeenAt, &p.ProfileVisibility, &p.ColVisibility,
		&p.LastSyncedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileNotSynced
	}
	if err != nil {
		return nil, fmt.Errorf("query profile: %w", err)
	}
	return &p, nil
}
