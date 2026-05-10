package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/historyevent"
)

// History event kinds. More can be added as new event sources land.
const (
	// HistoryKindIngest records an ingest of a recording into the catalog.
	HistoryKindIngest = "ingest"
	// HistoryKindRename records a rename/move applied to a recording's files.
	HistoryKindRename = "rename"
	// HistoryKindNFOWrite records writing a Jellyfin movie.nfo file.
	HistoryKindNFOWrite = "nfo_write"
	// HistoryKindEncoraPush records a format change pushed back to Encora.
	HistoryKindEncoraPush = "encora_push"
	// HistoryKindSync records a sync run against the Encora API.
	HistoryKindSync = "sync"
	// HistoryKindManualImport records a manual import performed by the user.
	HistoryKindManualImport = "manual_import"
)

// defaultHistoryListLimit is applied when ListHistoryOptions.Limit is zero.
const defaultHistoryListLimit = 100

// HistoryEvent is one row in the append-only history log.
type HistoryEvent struct {
	ID          int64
	OccurredAt  time.Time
	Kind        string
	RecordingID *int64
	Summary     string
	Details     map[string]any
}

// ListHistoryOptions filters and paginates a ListHistory query.
//
// Zero-valued time fields mean unbounded on that side. Limit defaults to 100
// when zero.
type ListHistoryOptions struct {
	Kinds       []string
	RecordingID *int64
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

// RecordEvent inserts a history event and returns its row id.
//
// If e.OccurredAt is the zero value, it is set to time.Now().UTC(). If
// e.Details is nil, an empty JSON object is stored.
func RecordEvent(ctx context.Context, client *ent.Client, e HistoryEvent) (int64, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}

	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return 0, fmt.Errorf("marshal history details: %w", err)
	}

	create := client.HistoryEvent.Create().
		SetOccurredAt(e.OccurredAt.UTC()).
		SetKind(e.Kind).
		SetSummary(e.Summary).
		SetDetailsJSON(string(detailsJSON))
	if e.RecordingID != nil {
		create = create.SetRecordingID(*e.RecordingID)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("insert history: %w", err)
	}
	return int64(row.ID), nil
}

// ListHistory returns history rows matching opts, ordered by occurred_at
// DESC, id DESC.
func ListHistory(
	ctx context.Context, client *ent.Client, opts ListHistoryOptions,
) ([]HistoryEvent, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = defaultHistoryListLimit
	}

	q := client.HistoryEvent.Query()
	if len(opts.Kinds) > 0 {
		q = q.Where(historyevent.KindIn(opts.Kinds...))
	}
	if opts.RecordingID != nil {
		q = q.Where(historyevent.RecordingID(*opts.RecordingID))
	}
	if !opts.Since.IsZero() {
		q = q.Where(historyevent.OccurredAtGTE(opts.Since.UTC()))
	}
	if !opts.Until.IsZero() {
		q = q.Where(historyevent.OccurredAtLT(opts.Until.UTC()))
	}

	rows, err := q.
		Order(
			historyevent.ByOccurredAt(entsql.OrderDesc()),
			historyevent.ByID(entsql.OrderDesc()),
		).
		Limit(limit).
		Offset(opts.Offset).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}

	out := make([]HistoryEvent, 0, len(rows))
	for _, r := range rows {
		ev := HistoryEvent{
			ID:         int64(r.ID),
			OccurredAt: r.OccurredAt.UTC(),
			Kind:       r.Kind,
			Summary:    r.Summary,
			Details:    map[string]any{},
		}
		if r.RecordingID != nil {
			id := *r.RecordingID
			ev.RecordingID = &id
		}
		raw := r.DetailsJSON
		if raw == "" {
			raw = "{}"
		}
		if uerr := json.Unmarshal([]byte(raw), &ev.Details); uerr != nil {
			return nil, fmt.Errorf("unmarshal history details: %w", uerr)
		}
		out = append(out, ev)
	}
	if out == nil {
		out = []HistoryEvent{}
	}
	// Preserve previous behavior — ListHistory used to return a nil slice
	// when there were no rows; callers handle both. Returning a non-nil
	// empty slice is also acceptable (tests assert via len/equals).
	return out, nil
}

// PruneHistoryOlderThan deletes history rows with occurred_at < t and returns
// the number of rows removed.
func PruneHistoryOlderThan(ctx context.Context, client *ent.Client, t time.Time) (int64, error) {
	n, err := client.HistoryEvent.Delete().
		Where(historyevent.OccurredAtLT(t.UTC())).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("prune history: %w", err)
	}
	return int64(n), nil
}
