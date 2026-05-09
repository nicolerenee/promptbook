package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrPerformerNotFound signals that no performer with the given id is in the
// local cache.
var ErrPerformerNotFound = errors.New("storage: performer not found")

// ErrCharacterNotFound signals that no character with the given id is in the
// local cache.
var ErrCharacterNotFound = errors.New("storage: character not found")

// Performer is the first-class people-table representation of an Encora actor.
//
// PerformerID is the Encora-supplied id, mirroring the convention used by
// recordings.recording_id and shows.show_id.
type Performer struct {
	PerformerID int64
	Name        string
	Slug        string
	URL         string
	LastSeenAt  time.Time
}

// Character is the first-class people-table representation of an Encora role.
type Character struct {
	CharacterID int64
	Name        string
	Slug        string
	URL         string
	LastSeenAt  time.Time
}

// UpsertPerformer inserts the performer or refreshes an existing row, keeping
// the latest name/slug/url and bumping last_seen_at to the supplied value.
func UpsertPerformer(ctx context.Context, db *sql.DB, p Performer) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO performers (performer_id, name, slug, url, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(performer_id) DO UPDATE SET
			name         = excluded.name,
			slug         = excluded.slug,
			url          = excluded.url,
			last_seen_at = excluded.last_seen_at
	`, p.PerformerID, p.Name, p.Slug, p.URL, p.LastSeenAt)
	if err != nil {
		return fmt.Errorf("upsert performer %d: %w", p.PerformerID, err)
	}
	return nil
}

// UpsertCharacter inserts the character or refreshes an existing row, keeping
// the latest name/slug/url and bumping last_seen_at to the supplied value.
func UpsertCharacter(ctx context.Context, db *sql.DB, c Character) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO characters (character_id, name, slug, url, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(character_id) DO UPDATE SET
			name         = excluded.name,
			slug         = excluded.slug,
			url          = excluded.url,
			last_seen_at = excluded.last_seen_at
	`, c.CharacterID, c.Name, c.Slug, c.URL, c.LastSeenAt)
	if err != nil {
		return fmt.Errorf("upsert character %d: %w", c.CharacterID, err)
	}
	return nil
}

// LoadPerformer fetches a single performer by Encora id, returning
// ErrPerformerNotFound when the row is missing.
func LoadPerformer(ctx context.Context, db *sql.DB, id int64) (*Performer, error) {
	p := Performer{PerformerID: id}
	err := db.QueryRowContext(ctx, `
		SELECT name, slug, url, last_seen_at
		FROM performers
		WHERE performer_id = ?
	`, id).Scan(&p.Name, &p.Slug, &p.URL, &p.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("performer %d: %w", id, ErrPerformerNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query performer %d: %w", id, err)
	}
	return &p, nil
}

// LoadCharacter fetches a single character by Encora id, returning
// ErrCharacterNotFound when the row is missing.
func LoadCharacter(ctx context.Context, db *sql.DB, id int64) (*Character, error) {
	c := Character{CharacterID: id}
	err := db.QueryRowContext(ctx, `
		SELECT name, slug, url, last_seen_at
		FROM characters
		WHERE character_id = ?
	`, id).Scan(&c.Name, &c.Slug, &c.URL, &c.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("character %d: %w", id, ErrCharacterNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query character %d: %w", id, err)
	}
	return &c, nil
}

// ListRecordingsForPerformer returns the distinct recording ids the given
// performer appears in, sorted ascending. An empty slice (not nil) is returned
// when the performer has no cast entries.
func ListRecordingsForPerformer(
	ctx context.Context,
	db *sql.DB,
	performerID int64,
) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT recording_id
		FROM cast_entries
		WHERE performer_id = ?
		ORDER BY recording_id
	`, performerID)
	if err != nil {
		return nil, fmt.Errorf("query recordings for performer %d: %w", performerID, err)
	}
	defer func() { _ = rows.Close() }()

	ids := []int64{}
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("scan recording id: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recordings for performer %d: %w", performerID, err)
	}
	return ids, nil
}
