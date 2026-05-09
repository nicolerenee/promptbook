package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
func RecordEvent(ctx context.Context, db *sql.DB, e HistoryEvent) (int64, error) {
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

	const q = `
INSERT INTO history (occurred_at, kind, recording_id, summary, details_json)
VALUES (?, ?, ?, ?, ?)
`
	res, err := db.ExecContext(ctx, q,
		e.OccurredAt.UTC(),
		e.Kind,
		nullableInt64(e.RecordingID),
		e.Summary,
		string(detailsJSON),
	)
	if err != nil {
		return 0, fmt.Errorf("insert history: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("history last insert id: %w", err)
	}
	return id, nil
}

// ListHistory returns history rows matching opts, ordered by occurred_at
// DESC, id DESC.
func ListHistory(ctx context.Context, db *sql.DB, opts ListHistoryOptions) ([]HistoryEvent, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = defaultHistoryListLimit
	}

	var (
		where []string
		args  []any
	)

	if len(opts.Kinds) > 0 {
		placeholders := make([]string, len(opts.Kinds))
		for i, k := range opts.Kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		where = append(where, "kind IN ("+strings.Join(placeholders, ",")+")")
	}

	if opts.RecordingID != nil {
		where = append(where, "recording_id = ?")
		args = append(args, *opts.RecordingID)
	}

	if !opts.Since.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, opts.Since.UTC())
	}

	if !opts.Until.IsZero() {
		where = append(where, "occurred_at < ?")
		args = append(args, opts.Until.UTC())
	}

	q := "SELECT id, occurred_at, kind, recording_id, summary, details_json FROM history"
	if len(where) > 0 {
		// where[] is composed of static fragments only (column names + "?" placeholders);
		// user values bind via args.
		//nolint:gosec // G202: see comment above; no user input in the concatenated string.
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, opts.Offset)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HistoryEvent
	for rows.Next() {
		var (
			ev          HistoryEvent
			recordingID sql.NullInt64
			detailsRaw  string
			occurredAt  time.Time
		)
		if scanErr := rows.Scan(&ev.ID, &occurredAt, &ev.Kind, &recordingID, &ev.Summary, &detailsRaw); scanErr != nil {
			return nil, fmt.Errorf("scan history row: %w", scanErr)
		}
		ev.OccurredAt = occurredAt.UTC()
		if recordingID.Valid {
			id := recordingID.Int64
			ev.RecordingID = &id
		}
		if detailsRaw == "" {
			detailsRaw = "{}"
		}
		ev.Details = map[string]any{}
		if unmarshalErr := json.Unmarshal([]byte(detailsRaw), &ev.Details); unmarshalErr != nil {
			return nil, fmt.Errorf("unmarshal history details: %w", unmarshalErr)
		}
		out = append(out, ev)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate history rows: %w", rowsErr)
	}
	return out, nil
}

// PruneHistoryOlderThan deletes history rows with occurred_at < t and returns
// the number of rows removed.
func PruneHistoryOlderThan(ctx context.Context, db *sql.DB, t time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, "DELETE FROM history WHERE occurred_at < ?", t.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("history rows affected: %w", err)
	}
	return n, nil
}

// nullableInt64 turns a *int64 into a sql.NullInt64 for parameter binding.
func nullableInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}
