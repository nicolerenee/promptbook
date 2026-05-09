package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Confidence labels for the scanner's auto-suggested Encora ID. Empty
// string ("") means the scanner had no suggestion at all — i.e. the file
// is unsuggested and a human will have to pick from scratch.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// ErrQueueEntryNotFound signals that no manual_import_queue row matches
// the given id. Returned by LoadQueueEntry; callers can use errors.Is to
// distinguish "not in queue" from a deeper SQL failure.
var ErrQueueEntryNotFound = errors.New("storage: manual import queue entry not found")

// QueueEntry is one row in the manual_import_queue table — a file the
// watched-folder scanner couldn't auto-resolve to an Encora recording.
// SuggestedRecordingID is a pointer because the scanner may have no
// suggestion at all.
type QueueEntry struct {
	ID                   int64
	FilePath             string
	FileSizeBytes        int64
	DiscoveredAt         time.Time
	LastSeenAt           time.Time
	SuggestedRecordingID *int64
	SuggestedConfidence  string
	Notes                string
}

// EnqueueFile inserts e into the queue, or updates the existing row if
// one already matches e.FilePath. discovered_at is preserved across
// updates — only the scanner's first sighting counts as discovery —
// while file_size_bytes, last_seen_at, suggested_recording_id,
// suggested_confidence, and notes all refresh to e's values.
//
// Returns the row id (newly inserted or pre-existing).
func EnqueueFile(ctx context.Context, db *sql.DB, e QueueEntry) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO manual_import_queue (
			file_path,
			file_size_bytes,
			last_seen_at,
			suggested_recording_id,
			suggested_confidence,
			notes
		) VALUES (?, ?, CURRENT_TIMESTAMP, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			file_size_bytes        = excluded.file_size_bytes,
			last_seen_at           = CURRENT_TIMESTAMP,
			suggested_recording_id = excluded.suggested_recording_id,
			suggested_confidence   = excluded.suggested_confidence,
			notes                  = excluded.notes
	`, e.FilePath, e.FileSizeBytes, nullableInt64(e.SuggestedRecordingID), e.SuggestedConfidence, e.Notes)
	if err != nil {
		return 0, fmt.Errorf("upsert manual_import_queue: %w", err)
	}

	// LastInsertId returns the rowid of the affected row on both INSERT
	// and ON CONFLICT DO UPDATE in SQLite, but to be defensive (and to
	// match the contract callers expect — "id of the row, new or old")
	// we re-query by file_path on the update path. In practice
	// LastInsertId is correct here, but a SELECT is cheap and explicit.
	if rows, raffErr := res.RowsAffected(); raffErr == nil && rows > 0 {
		if id, lerr := res.LastInsertId(); lerr == nil && id > 0 {
			// Verify the row id matches what's actually stored — on
			// SQLite UPSERT, LastInsertId can lag the conflict path.
			var storedID int64
			if qerr := db.QueryRowContext(ctx, `
				SELECT id FROM manual_import_queue WHERE file_path = ?
			`, e.FilePath).Scan(&storedID); qerr == nil {
				return storedID, nil
			}
			return id, nil
		}
	}

	var id int64
	if err = db.QueryRowContext(ctx, `
		SELECT id FROM manual_import_queue WHERE file_path = ?
	`, e.FilePath).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup manual_import_queue id: %w", err)
	}
	return id, nil
}

// RemoveQueueEntry deletes the queue row with the given id. Used after a
// human resolves an entry to an Encora recording. Returns nil if the
// row didn't exist — removal is idempotent from the caller's view.
func RemoveQueueEntry(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `
		DELETE FROM manual_import_queue WHERE id = ?
	`, id); err != nil {
		return fmt.Errorf("delete manual_import_queue id %d: %w", id, err)
	}
	return nil
}

// RemoveQueueEntryByPath deletes the queue row whose file_path matches.
// Useful for the scanner: when a file disappears between sweeps, we
// drop its queue entry without needing to remember the row id.
func RemoveQueueEntryByPath(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, `
		DELETE FROM manual_import_queue WHERE file_path = ?
	`, path); err != nil {
		return fmt.Errorf("delete manual_import_queue path %q: %w", path, err)
	}
	return nil
}

// ListQueue returns all queue rows in (discovered_at ASC, id ASC) order
// — oldest-first, with id as the tiebreaker for entries enqueued in the
// same CURRENT_TIMESTAMP tick. Returns an empty (non-nil) slice when
// the table is empty so callers can range over it safely.
func ListQueue(ctx context.Context, db *sql.DB) ([]QueueEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, file_path, file_size_bytes, discovered_at, last_seen_at,
		       suggested_recording_id, suggested_confidence, notes
		FROM manual_import_queue
		ORDER BY discovered_at ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query manual_import_queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]QueueEntry, 0)
	for rows.Next() {
		entry, scanErr := scanQueueEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manual_import_queue: %w", err)
	}
	return out, nil
}

// LoadQueueEntry fetches a single queue row by id. Returns
// ErrQueueEntryNotFound if no row matches.
func LoadQueueEntry(ctx context.Context, db *sql.DB, id int64) (*QueueEntry, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, file_path, file_size_bytes, discovered_at, last_seen_at,
		       suggested_recording_id, suggested_confidence, notes
		FROM manual_import_queue
		WHERE id = ?
	`, id)
	entry, err := scanQueueEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("queue entry %d: %w", id, ErrQueueEntryNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// scanRow narrows database/sql's *Row and *Rows down to the one method
// scanQueueEntry actually needs, so it can scan from either.
type scanRow interface {
	Scan(dest ...any) error
}

// scanQueueEntry reads one queue row from a *sql.Row or *sql.Rows. Wraps
// the SQL error with context so callers see "scan manual_import_queue:"
// in the stack rather than a bare driver error.
func scanQueueEntry(row scanRow) (QueueEntry, error) {
	var (
		entry        QueueEntry
		suggestedID  sql.NullInt64
		discoveredAt time.Time
		lastSeenAt   time.Time
	)
	if err := row.Scan(
		&entry.ID,
		&entry.FilePath,
		&entry.FileSizeBytes,
		&discoveredAt,
		&lastSeenAt,
		&suggestedID,
		&entry.SuggestedConfidence,
		&entry.Notes,
	); err != nil {
		// Bubble sql.ErrNoRows through unwrapped so callers can errors.Is
		// it; only wrap other errors with our context.
		if errors.Is(err, sql.ErrNoRows) {
			return QueueEntry{}, err
		}
		return QueueEntry{}, fmt.Errorf("scan manual_import_queue: %w", err)
	}
	entry.DiscoveredAt = discoveredAt
	entry.LastSeenAt = lastSeenAt
	if suggestedID.Valid {
		v := suggestedID.Int64
		entry.SuggestedRecordingID = &v
	}
	return entry, nil
}
