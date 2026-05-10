package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/externalids"
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
// sqlDB, when non-nil, is also used to upsert the recording's Encora id
// into the external_ids table — keeping the external_ids surface uniform
// across legacy (migration back-fill) and new (this hook) Encora rows.
// Passing nil skips the upsert; the next migration cycle's back-fill is
// scoped to existing rows so a missed hook leaves the table missing
// only the freshly-fetched row, which subsequent ListForRecording calls
// still find via the recordings.id-as-Encora-id semantics until Phase L
// swaps the PK shape.
//
// now defaults to time.Now when nil.
func PersistRecording(
	ctx context.Context,
	client *ent.Client,
	sqlDB *sql.DB,
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
	// Stamp the Encora row into external_ids so the table is the
	// uniform source of truth across providers. UpsertMany is
	// idempotent under the composite PK — repeat calls for the same
	// recording are a no-op.
	if sqlDB != nil {
		if upsertErr := externalids.UpsertMany(ctx, sqlDB, []externalids.ExternalID{{
			RecordingID: r.ID,
			Provider:    externalids.ProviderEncora,
			ExternalID:  externalids.EncoraID(r.ID),
		}}); upsertErr != nil {
			return fmt.Errorf("upsert external_ids encora row %d: %w", r.ID, upsertErr)
		}
	}
	return nil
}
