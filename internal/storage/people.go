package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/character"
	"github.com/nicolerenee/promptbook/internal/ent/performer"
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
func UpsertPerformer(ctx context.Context, client *ent.Client, p Performer) error {
	return upsertPerformer(ctx, client.Performer.Create(), p)
}

// UpsertPerformerTx is the transaction-scoped sibling of UpsertPerformer for
// callers that already hold an *ent.Tx (e.g. the sync writer batching cast
// upserts inside the per-page transaction).
func UpsertPerformerTx(ctx context.Context, tx *ent.Tx, p Performer) error {
	return upsertPerformer(ctx, tx.Performer.Create(), p)
}

func upsertPerformer(
	ctx context.Context, c *ent.PerformerCreate, p Performer,
) error {
	err := c.
		SetID(p.PerformerID).
		SetName(p.Name).
		SetSlug(p.Slug).
		SetURL(p.URL).
		SetLastSeenAt(p.LastSeenAt).
		OnConflictColumns(performer.FieldID).
		Update(func(u *ent.PerformerUpsert) {
			u.UpdateName()
			u.UpdateSlug()
			u.UpdateURL()
			u.UpdateLastSeenAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert performer %d: %w", p.PerformerID, err)
	}
	return nil
}

// UpsertCharacter inserts the character or refreshes an existing row, keeping
// the latest name/slug/url and bumping last_seen_at to the supplied value.
func UpsertCharacter(ctx context.Context, client *ent.Client, c Character) error {
	return upsertCharacter(ctx, client.Character.Create(), c)
}

// UpsertCharacterTx is the transaction-scoped sibling of UpsertCharacter for
// callers that already hold an *ent.Tx.
func UpsertCharacterTx(ctx context.Context, tx *ent.Tx, c Character) error {
	return upsertCharacter(ctx, tx.Character.Create(), c)
}

func upsertCharacter(
	ctx context.Context, b *ent.CharacterCreate, c Character,
) error {
	err := b.
		SetID(c.CharacterID).
		SetName(c.Name).
		SetSlug(c.Slug).
		SetURL(c.URL).
		SetLastSeenAt(c.LastSeenAt).
		OnConflictColumns(character.FieldID).
		Update(func(u *ent.CharacterUpsert) {
			u.UpdateName()
			u.UpdateSlug()
			u.UpdateURL()
			u.UpdateLastSeenAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert character %d: %w", c.CharacterID, err)
	}
	return nil
}

// LoadPerformer fetches a single performer by Encora id, returning
// ErrPerformerNotFound when the row is missing.
func LoadPerformer(ctx context.Context, client *ent.Client, id int64) (*Performer, error) {
	row, err := client.Performer.Get(ctx, id)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("performer %d: %w", id, ErrPerformerNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query performer %d: %w", id, err)
	}
	return &Performer{
		PerformerID: row.ID,
		Name:        row.Name,
		Slug:        row.Slug,
		URL:         row.URL,
		LastSeenAt:  row.LastSeenAt,
	}, nil
}

// LoadCharacter fetches a single character by Encora id, returning
// ErrCharacterNotFound when the row is missing.
func LoadCharacter(ctx context.Context, client *ent.Client, id int64) (*Character, error) {
	row, err := client.Character.Get(ctx, id)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("character %d: %w", id, ErrCharacterNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query character %d: %w", id, err)
	}
	return &Character{
		CharacterID: row.ID,
		Name:        row.Name,
		Slug:        row.Slug,
		URL:         row.URL,
		LastSeenAt:  row.LastSeenAt,
	}, nil
}

// ListRecordingsForPerformer returns the distinct recording ids the given
// performer appears in, sorted ascending. An empty slice (not nil) is returned
// when the performer has no cast entries.
func ListRecordingsForPerformer(
	ctx context.Context,
	client *ent.Client,
	performerID int64,
) ([]int64, error) {
	// SELECT DISTINCT recording_id FROM cast_entries WHERE performer_id = ?
	// ORDER BY recording_id — ent's GroupBy on a single column returns a
	// distinct, ordered slice when followed by Strings/Ints. For int64
	// scalars we use a typed scan via the ent ScanX helper.
	var ids []int64
	err := client.CastEntry.Query().
		Where(castentry.PerformerID(performerID)).
		GroupBy(castentry.FieldRecordingID).
		Scan(ctx, &ids)
	if err != nil {
		return nil, fmt.Errorf("query recordings for performer %d: %w", performerID, err)
	}
	if ids == nil {
		ids = []int64{}
	}
	// Stable ascending sort: ent's GroupBy doesn't guarantee order across
	// dialects, so sort here to match the legacy SQL ORDER BY recording_id.
	sortAscInt64(ids)
	return ids, nil
}

// sortAscInt64 sorts in place; small slices so insertion sort is fine
// without pulling in sort.Slice. Used by ListRecordingsForPerformer.
func sortAscInt64(s []int64) {
	for i := 1; i < len(s); i++ {
		v := s[i]
		j := i - 1
		for j >= 0 && s[j] > v {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = v
	}
}
