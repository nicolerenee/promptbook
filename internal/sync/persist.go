package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
)

// PersistRecording writes a single recording into shows/recordings/cast_entries
// without going through a full Sync run. It's used by ingest to land
// auto-fetched single-recording detail (`/recording/{id}`) when the local DB
// doesn't yet know about an id.
//
// The recording lands as an "orphan" — there's no collection or wants row
// inserted, since neither membership has been established. A later
// `promptbook collection sync` (or an explicit AddToCollection POST) is what
// transitions it into the user's collection.
//
// now defaults to time.Now when nil.
func PersistRecording(
	ctx context.Context,
	client *ent.Client,
	r encora.Recording,
	now func() time.Time,
) error {
	if now == nil {
		now = time.Now
	}
	tx, err := client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if uerr := upsertRecording(ctx, tx, r, now); uerr != nil {
		return fmt.Errorf("upsert recording %d: %w", r.ID, uerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("commit tx: %w", cerr)
	}
	return nil
}
