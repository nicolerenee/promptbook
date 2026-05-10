package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/manualimportqueue"
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
// suggestion at all. ExtrasCount is the number of OTHER media files in
// the same source folder when the row represents a folder-as-unit
// (i.e. FilePath is the "main" file living next to per-track audio rips
// or photos). 0 for loose-file rows at the watched-dir root.
type QueueEntry struct {
	ID                   int64
	FilePath             string
	FileSizeBytes        int64
	DiscoveredAt         time.Time
	LastSeenAt           time.Time
	SuggestedRecordingID *int64
	SuggestedConfidence  string
	Notes                string
	ExtrasCount          int
	// ClassificationJSON is the scanner's per-file role classification
	// for this folder-as-unit drop, JSON-encoded so the queue import
	// modal can render the multi-file picker without re-walking the
	// folder. Empty string for legacy rows or for loose-file enqueues.
	ClassificationJSON string
}

// EnqueueFile inserts e into the queue, or updates the existing row if
// one already matches e.FilePath. discovered_at is preserved across
// updates — only the scanner's first sighting counts as discovery —
// while file_size_bytes, last_seen_at, suggested_recording_id,
// suggested_confidence, and notes all refresh to e's values.
//
// Returns the row id (newly inserted or pre-existing).
func EnqueueFile(ctx context.Context, client *ent.Client, e QueueEntry) (int64, error) {
	now := time.Now().UTC()
	create := client.ManualImportQueue.Create().
		SetFilePath(e.FilePath).
		SetFileSizeBytes(e.FileSizeBytes).
		SetLastSeenAt(now).
		SetSuggestedConfidence(e.SuggestedConfidence).
		SetNotes(e.Notes).
		SetExtrasCount(e.ExtrasCount).
		SetClassificationJSON(e.ClassificationJSON)
	if e.SuggestedRecordingID != nil {
		create = create.SetSuggestedRecordingID(*e.SuggestedRecordingID)
	}
	err := create.
		OnConflictColumns(manualimportqueue.FieldFilePath).
		Update(func(u *ent.ManualImportQueueUpsert) {
			u.UpdateFileSizeBytes()
			u.SetLastSeenAt(now)
			if e.SuggestedRecordingID != nil {
				u.SetSuggestedRecordingID(*e.SuggestedRecordingID)
			} else {
				u.ClearSuggestedRecordingID()
			}
			u.UpdateSuggestedConfidence()
			u.UpdateNotes()
			u.UpdateExtrasCount()
			u.UpdateClassificationJSON()
		}).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("upsert manual_import_queue: %w", err)
	}

	row, err := client.ManualImportQueue.Query().
		Where(manualimportqueue.FilePath(e.FilePath)).
		Only(ctx)
	if err != nil {
		return 0, fmt.Errorf("lookup manual_import_queue id: %w", err)
	}
	return int64(row.ID), nil
}

// RemoveQueueEntry deletes the queue row with the given id. Used after a
// human resolves an entry to an Encora recording. Returns nil if the
// row didn't exist — removal is idempotent from the caller's view.
func RemoveQueueEntry(ctx context.Context, client *ent.Client, id int64) error {
	_, err := client.ManualImportQueue.Delete().
		Where(manualimportqueue.IDEQ(int(id))).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete manual_import_queue id %d: %w", id, err)
	}
	return nil
}

// RemoveQueueEntryByPath deletes the queue row whose file_path matches.
// Useful for the scanner: when a file disappears between sweeps, we
// drop its queue entry without needing to remember the row id.
func RemoveQueueEntryByPath(ctx context.Context, client *ent.Client, path string) error {
	_, err := client.ManualImportQueue.Delete().
		Where(manualimportqueue.FilePath(path)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete manual_import_queue path %q: %w", path, err)
	}
	return nil
}

// ListQueue returns all queue rows in (discovered_at ASC, id ASC) order
// — oldest-first, with id as the tiebreaker for entries enqueued in the
// same CURRENT_TIMESTAMP tick. Returns an empty (non-nil) slice when
// the table is empty so callers can range over it safely.
func ListQueue(ctx context.Context, client *ent.Client) ([]QueueEntry, error) {
	rows, err := client.ManualImportQueue.Query().
		Order(
			manualimportqueue.ByDiscoveredAt(),
			manualimportqueue.ByID(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query manual_import_queue: %w", err)
	}
	out := make([]QueueEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, queueEntryFromEnt(r))
	}
	return out, nil
}

// LoadQueueEntry fetches a single queue row by id. Returns
// ErrQueueEntryNotFound if no row matches.
func LoadQueueEntry(ctx context.Context, client *ent.Client, id int64) (*QueueEntry, error) {
	row, err := client.ManualImportQueue.Get(ctx, int(id))
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("queue entry %d: %w", id, ErrQueueEntryNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query manual_import_queue %d: %w", id, err)
	}
	out := queueEntryFromEnt(row)
	return &out, nil
}

func queueEntryFromEnt(r *ent.ManualImportQueue) QueueEntry {
	entry := QueueEntry{
		ID:                  int64(r.ID),
		FilePath:            r.FilePath,
		FileSizeBytes:       r.FileSizeBytes,
		DiscoveredAt:        r.DiscoveredAt,
		LastSeenAt:          r.LastSeenAt,
		SuggestedConfidence: r.SuggestedConfidence,
		Notes:               r.Notes,
		ExtrasCount:         r.ExtrasCount,
		ClassificationJSON:  r.ClassificationJSON,
	}
	if r.SuggestedRecordingID != nil {
		v := *r.SuggestedRecordingID
		entry.SuggestedRecordingID = &v
	}
	return entry
}
