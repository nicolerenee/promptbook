package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

// Store wraps the job_runs and job_state tables. Every Runner
// state transition writes through Store so an unexpected crash leaves
// the on-disk record honest: a row marked StatusRunning at the time
// of the crash stays StatusRunning after restart and the operator can
// spot stuck jobs in the UI.
//
// Store is safe for concurrent use because the underlying *sql.DB
// pool is single-connection (see internal/storage.Open) — SQLite
// serializes writes for us.
type Store struct {
	db *sql.DB
}

// NewStore wraps db. db must already have the migrations from
// 00008_jobs.sql applied.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// InsertRun persists a freshly-queued Run and returns its assigned
// ID. The caller is expected to update *r.ID with the returned value
// before passing the pointer down to the worker pool. Args is
// encoded as canonical JSON; nil/empty Args writes the empty string
// so the no-args path is visually distinct in the database.
func (s *Store) InsertRun(ctx context.Context, r *Run) (int64, error) {
	argsJSON := canonicalArgs(r.Args)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO job_runs (job_name, queued_at, status, trigger, error, args)
		VALUES (?, ?, ?, ?, '', ?)
	`, r.JobName, r.QueuedAt, string(r.Status), string(r.Trigger), argsJSON)
	if err != nil {
		return 0, fmt.Errorf("insert job_run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// MarkStarted bumps a Run from queued → running and records when the
// worker picked it up.
func (s *Store) MarkStarted(ctx context.Context, runID int64, startedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs SET started_at = ?, status = ? WHERE id = ?
	`, startedAt, string(StatusRunning), runID)
	if err != nil {
		return fmt.Errorf("mark started: %w", err)
	}
	return nil
}

// MarkEnded finalizes a Run with its terminal status and (for
// failures) the error string. Same call upserts job_state so the next
// scheduling decision and the ScheduledView can both read the
// canonical "last run" without rescanning job_runs.
func (s *Store) MarkEnded(
	ctx context.Context, runID int64,
	jobName string, startedAt, endedAt time.Time,
	status Status, errText string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, `
		UPDATE job_runs SET ended_at = ?, status = ?, error = ? WHERE id = ?
	`, endedAt, string(status), errText, runID); err != nil {
		return fmt.Errorf("mark ended: %w", err)
	}

	durMs := int64(0)
	if !startedAt.IsZero() {
		durMs = endedAt.Sub(startedAt).Milliseconds()
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO job_state (
			job_name, last_started_at, last_ended_at,
			last_duration_ms, last_status
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_name) DO UPDATE SET
			last_started_at  = excluded.last_started_at,
			last_ended_at    = excluded.last_ended_at,
			last_duration_ms = excluded.last_duration_ms,
			last_status      = excluded.last_status
	`, jobName, startedAt, endedAt, durMs, string(status)); err != nil {
		return fmt.Errorf("upsert job_state: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// loadState pulls the persisted row for jobName. ErrNoState is
// returned when the row does not exist, so callers can distinguish
// "first ever run" from "row corrupt". The return value carries
// unexported fields because callers inside this package read them
// directly; outside the package the Runner's ScheduledView covers
// the read needs.
func (s *Store) loadState(ctx context.Context, jobName string) (jobState, error) {
	var (
		st                 jobState
		startedAt, endedAt sql.NullTime
		durationMs         sql.NullInt64
		lastStatus         sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT last_started_at, last_ended_at, last_duration_ms, last_status
		FROM job_state WHERE job_name = ?
	`, jobName).Scan(&startedAt, &endedAt, &durationMs, &lastStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return jobState{}, ErrNoState
	}
	if err != nil {
		return jobState{}, fmt.Errorf("load job_state: %w", err)
	}
	if startedAt.Valid {
		t := startedAt.Time
		st.lastStartedAt = &t
	}
	if endedAt.Valid {
		t := endedAt.Time
		st.lastEndedAt = &t
	}
	if durationMs.Valid {
		st.lastDuration = time.Duration(durationMs.Int64) * time.Millisecond
	}
	if lastStatus.Valid {
		st.lastStatus = Status(lastStatus.String)
	}
	return st, nil
}

// ListRecent returns the newest limit Runs across every job, ordered
// queued_at desc. The Runner's REST handler uses it to populate the
// /jobs Queue section; limits are bounded by the caller. Args is
// decoded from the JSON column; an invalid blob is logged and
// surfaced as nil so a corrupt row doesn't poison the whole list.
func (s *Store) ListRecent(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, queued_at, started_at, ended_at,
		       status, COALESCE(error, ''), trigger, COALESCE(args, '')
		FROM job_runs
		ORDER BY queued_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Run, 0)
	for rows.Next() {
		var (
			r                  Run
			startedAt, endedAt sql.NullTime
			status, trigger    string
			argsJSON           string
		)
		if scanErr := rows.Scan(
			&r.ID, &r.JobName, &r.QueuedAt,
			&startedAt, &endedAt, &status, &r.Error, &trigger, &argsJSON,
		); scanErr != nil {
			return nil, fmt.Errorf("scan run row: %w", scanErr)
		}
		if startedAt.Valid {
			r.StartedAt = startedAt.Time
		}
		if endedAt.Valid {
			r.EndedAt = endedAt.Time
		}
		r.Status = Status(status)
		r.Trigger = Trigger(trigger)
		r.Args = decodeArgs(argsJSON, r.ID)
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate run rows: %w", rerr)
	}
	return out, nil
}

// decodeArgs parses a job_runs.args blob. Empty string → nil (the
// no-args path); invalid JSON logs a warning and returns nil so the
// list keeps moving rather than hard-failing on a corrupt row.
func decodeArgs(argsJSON string, runID int64) JobArgs {
	if argsJSON == "" {
		return nil
	}
	var args JobArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		log.Warn().
			Err(err).
			Int64("run_id", runID).
			Str("args_raw", argsJSON).
			Msg("jobs: failed to decode persisted args; treating as empty")
		return nil
	}
	return args
}

// ErrNoState signals that a job_state row does not yet exist for the
// requested job name. Distinct from a query failure so callers can
// branch on "first run ever" without heuristics.
var ErrNoState = errors.New("jobs: no persisted state for job")

// jobState is the de-serialized form of a job_state row. Pointer
// timestamps mirror the nullable columns so a never-run job stays
// distinguishable from one that ran-and-completed-at-the-zero-time.
type jobState struct {
	lastStartedAt *time.Time
	lastEndedAt   *time.Time
	lastDuration  time.Duration
	lastStatus    Status
}
