// Package externalids is the normalized framework for third-party
// record ids (encora, tmdb, imdb, fanart.tv, …) attached to a
// promptbook recording.
//
// The `external_ids` table stores (recording_id, provider,
// external_id) — one row per (recording, provider) pair. Encora ids
// are back-filled at migration time so the table is the source of
// truth for "what's provider X's id for this local recording?" across
// every provider, including Encora.
//
// Phase L roadmap: today recordings.id doubles as the Encora id and
// is the recording's primary key. Phase L swaps that for a locally-
// generated ULID/nanoid, at which point this table is the only place
// the Encora id lives. Until then, every NEW code path that needs an
// external id for any provider goes through this package — not
// through recording.id directly. Old code that uses recording.id as
// the Encora id stays untouched; the Encora back-fill keeps the data
// uniform so the Phase L PK swap is a focused single-table refactor,
// not a 50-callsite rewrite.
package externalids

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Provider is the typed string for a third-party id source. New
// providers slot in by adding a constant + a URLFor / LabelFor branch
// below; the persistence layer never hard-codes a provider list.
type Provider string

// Known providers. Add new entries here when wiring in another id
// source — URLFor + LabelFor below carry the corresponding human-
// facing strings + canonical-link template.
const (
	// ProviderEncora is the Encora numeric recording id. Stored as
	// the decimal string form of the int64 (strconv.FormatInt) so the
	// TEXT column round-trips uniformly with the alphanumeric IMDB
	// ids.
	ProviderEncora Provider = "encora"
	// ProviderTMDB is the TMDB numeric movie id (digits only).
	ProviderTMDB Provider = "tmdb"
	// ProviderIMDB is the IMDB title id, with the canonical "tt"
	// prefix retained (e.g. "tt99999999"). The URL builder uses the
	// id as-is — no extra prefix.
	ProviderIMDB Provider = "imdb"
)

// ExternalID is one (recording_id, provider, external_id) row. Used
// as both the input shape for UpsertMany and the output shape for
// ListForRecording — the column set is small enough that a single
// struct serves both directions.
type ExternalID struct {
	RecordingID int64
	Provider    Provider
	ExternalID  string
}

// Execer is the subset of *sql.DB / *sql.Tx the persistence helpers
// need. Defined as an interface so callers can pass either a plain
// connection pool or an in-flight transaction without juggling
// generic-vs-tx overloads. Both *sql.DB and *sql.Tx satisfy this
// shape out of the box.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// EncoraID returns the Encora external id encoded as it lives in the
// external_ids.external_id TEXT column — strconv.FormatInt of the
// int64 recording id. Centralized in this helper so Phase L's PK
// swap (recordings.id → locally-generated ULID) only has to touch
// one place: every caller that today writes
// `strconv.FormatInt(rec.ID, 10)` will switch to
// externalids.EncoraID(rec.EncoraID()), and the swap becomes a one-
// helper edit instead of a tour of every call site.
func EncoraID(recordingID int64) string {
	return strconv.FormatInt(recordingID, 10)
}

// UpsertMany inserts (or upserts) every row in ids inside a single
// transaction. The (recording_id, provider) composite PK absorbs
// duplicates — re-running the call with the same shape leaves the
// table identical. Empty ids returns nil without opening the tx.
//
// All-or-nothing semantics: a row-level failure rolls back every
// preceding insert in the same call. This is the right tradeoff for
// the caller (the ingest engine wants either "every external id for
// this recording landed" or "none did" — partial state is harder to
// recover than retrying the full batch).
func UpsertMany(
	ctx context.Context, db *sql.DB, ids []ExternalID,
) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if upsertErr := upsertManyTx(ctx, tx, ids); upsertErr != nil {
		_ = tx.Rollback()
		return upsertErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("commit tx: %w", commitErr)
	}
	return nil
}

// UpsertManyTx is UpsertMany scoped to a caller-supplied transaction.
// Used by callers (sync.PersistRecording) that are already inside a
// tx and want the upsert to participate in the same atomic unit.
func UpsertManyTx(ctx context.Context, exec Execer, ids []ExternalID) error {
	return upsertManyTx(ctx, exec, ids)
}

// upsertManyTx does the per-row INSERT ... ON CONFLICT against the
// caller's execer (either *sql.Tx for UpsertMany or any Execer for
// UpsertManyTx).
func upsertManyTx(ctx context.Context, exec Execer, ids []ExternalID) error {
	const stmt = `INSERT INTO external_ids (recording_id, provider, external_id)
VALUES (?, ?, ?)
ON CONFLICT (recording_id, provider) DO UPDATE SET external_id = excluded.external_id`
	for _, id := range ids {
		if id.Provider == "" {
			return fmt.Errorf("upsert external id: empty provider for recording %d", id.RecordingID)
		}
		if id.ExternalID == "" {
			return fmt.Errorf("upsert external id: empty external_id for recording %d provider %q",
				id.RecordingID, id.Provider)
		}
		if _, err := exec.ExecContext(ctx, stmt, id.RecordingID, string(id.Provider), id.ExternalID); err != nil {
			return fmt.Errorf("upsert external id (rec=%d provider=%s): %w",
				id.RecordingID, id.Provider, err)
		}
	}
	return nil
}

// ListForRecording returns every external_ids row for the recording,
// ordered by provider so the SPA's chip row renders deterministically
// (encora · tmdb · imdb in alphabetical order; a chip-row sort
// override at the call site can rearrange if needed). Empty slice +
// nil error when the recording has no rows.
func ListForRecording(
	ctx context.Context, db *sql.DB, recordingID int64,
) ([]ExternalID, error) {
	const stmt = `SELECT recording_id, provider, external_id
FROM external_ids
WHERE recording_id = ?
ORDER BY provider ASC`
	rows, err := db.QueryContext(ctx, stmt, recordingID)
	if err != nil {
		return nil, fmt.Errorf("query external_ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ExternalID, 0)
	for rows.Next() {
		var row ExternalID
		var providerStr string
		if scanErr := rows.Scan(&row.RecordingID, &providerStr, &row.ExternalID); scanErr != nil {
			return nil, fmt.Errorf("scan external_ids row: %w", scanErr)
		}
		row.Provider = Provider(providerStr)
		out = append(out, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate external_ids rows: %w", rowsErr)
	}
	return out, nil
}

// FindRecordingByExternalID resolves a (provider, externalID) pair to
// the local recording id. Returns (id, true, nil) on a hit,
// (0, false, nil) when the pair isn't in the table, and (0, false, err)
// only on a real DB error.
//
// Used by callers that need "do I already have a local row for this
// upstream id?" — e.g. the scanner's folder-name parser when it pulls
// a TMDB id out of a Radarr-managed folder and wants to fast-path the
// queue suggestion.
func FindRecordingByExternalID(
	ctx context.Context, db *sql.DB, provider Provider, externalID string,
) (int64, bool, error) {
	const stmt = `SELECT recording_id
FROM external_ids
WHERE provider = ? AND external_id = ?
LIMIT 1`
	var id int64
	err := db.QueryRowContext(ctx, stmt, string(provider), externalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("query external_ids by pair: %w", err)
	}
	return id, true, nil
}

// URLFor returns the canonical public web URL for a provider's id, or
// "" for an unknown provider. The returned URL is safe to drop into
// an <a href=""> attribute without further escaping — the ids are
// path-segment-safe (digits or "tt"+digits).
func URLFor(provider Provider, externalID string) string {
	if externalID == "" {
		return ""
	}
	switch provider {
	case ProviderEncora:
		return "https://encora.it/recordings/" + externalID
	case ProviderTMDB:
		return "https://www.themoviedb.org/movie/" + externalID
	case ProviderIMDB:
		return "https://www.imdb.com/title/" + externalID
	default:
		return ""
	}
}

// LabelFor returns the human-readable provider name the SPA renders
// inside the chip. Returns the raw provider string for unknown
// providers so a misconfigured row still surfaces something readable
// rather than collapsing the chip entirely.
func LabelFor(provider Provider) string {
	switch provider {
	case ProviderEncora:
		return "Encora"
	case ProviderTMDB:
		return "TMDB"
	case ProviderIMDB:
		return "IMDB"
	default:
		return string(provider)
	}
}
