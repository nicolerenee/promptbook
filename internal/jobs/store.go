package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/rs/zerolog/log"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/jobrun"
	"github.com/nicolerenee/promptbook/internal/ent/jobstate"
)

// Store wraps the job_runs and job_state tables. Every Runner state
// transition writes through Store so an unexpected crash leaves the
// on-disk record honest: a row marked StatusRunning at the time of the
// crash stays StatusRunning after restart and the operator can spot
// stuck jobs in the UI.
//
// Store is safe for concurrent use because the underlying *ent.Client
// is — SQLite serializes writes for us under WAL.
type Store struct {
	db *ent.Client
}

// NewStore wraps client. The underlying connection must already have
// the migrations from 00008_jobs.sql applied (storage.OpenEnt does
// that).
func NewStore(client *ent.Client) *Store {
	return &Store{db: client}
}

// InsertRun persists a freshly-queued Run and returns its assigned ID.
// The caller is expected to update *r.ID with the returned value
// before passing the pointer down to the worker pool. Args is encoded
// as canonical JSON; nil/empty Args writes the empty string so the
// no-args path is visually distinct in the database.
func (s *Store) InsertRun(ctx context.Context, r *Run) (int64, error) {
	argsJSON := canonicalArgs(r.Args)
	row, err := s.db.JobRun.Create().
		SetJobName(r.JobName).
		SetQueuedAt(r.QueuedAt).
		SetStatus(string(r.Status)).
		SetTrigger(string(r.Trigger)).
		SetError("").
		SetArgs(argsJSON).
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("insert job_run: %w", err)
	}
	return int64(row.ID), nil
}

// MarkStarted bumps a Run from queued → running and records when the
// worker picked it up.
func (s *Store) MarkStarted(ctx context.Context, runID int64, startedAt time.Time) error {
	_, err := s.db.JobRun.UpdateOneID(int(runID)).
		SetStartedAt(startedAt).
		SetStatus(string(StatusRunning)).
		Save(ctx)
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
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.JobRun.UpdateOneID(int(runID)).
		SetEndedAt(endedAt).
		SetStatus(string(status)).
		SetError(errText).
		Save(ctx); err != nil {
		return fmt.Errorf("mark ended: %w", err)
	}

	durMs := int64(0)
	if !startedAt.IsZero() {
		durMs = endedAt.Sub(startedAt).Milliseconds()
	}
	if err = tx.JobState.Create().
		SetID(jobName).
		SetLastStartedAt(startedAt).
		SetLastEndedAt(endedAt).
		SetLastDurationMs(int(durMs)).
		SetLastStatus(string(status)).
		OnConflictColumns(jobstate.FieldID).
		Update(func(u *ent.JobStateUpsert) {
			u.UpdateLastStartedAt()
			u.UpdateLastEndedAt()
			u.UpdateLastDurationMs()
			u.UpdateLastStatus()
		}).
		Exec(ctx); err != nil {
		return fmt.Errorf("upsert job_state: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// loadState pulls the persisted row for jobName. ErrNoState is returned
// when the row does not exist, so callers can distinguish "first ever
// run" from "row corrupt".
func (s *Store) loadState(ctx context.Context, jobName string) (jobState, error) {
	row, err := s.db.JobState.Get(ctx, jobName)
	if ent.IsNotFound(err) {
		return jobState{}, ErrNoState
	}
	if err != nil {
		return jobState{}, fmt.Errorf("load job_state: %w", err)
	}
	st := jobState{}
	if row.LastStartedAt != nil {
		t := *row.LastStartedAt
		st.lastStartedAt = &t
	}
	if row.LastEndedAt != nil {
		t := *row.LastEndedAt
		st.lastEndedAt = &t
	}
	if row.LastDurationMs != nil {
		st.lastDuration = time.Duration(*row.LastDurationMs) * time.Millisecond
	}
	if row.LastStatus != nil {
		st.lastStatus = Status(*row.LastStatus)
	}
	return st, nil
}

// ListRecent returns the newest limit Runs across every job, ordered
// queued_at desc. The Runner's REST handler uses it to populate the
// /jobs Queue section; limits are bounded by the caller. Args is
// decoded from the JSON column; an invalid blob is logged and surfaced
// as nil so a corrupt row doesn't poison the whole list.
func (s *Store) ListRecent(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.JobRun.Query().
		Order(
			jobrun.ByQueuedAt(entsql.OrderDesc()),
			jobrun.ByID(entsql.OrderDesc()),
		).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		run := Run{
			ID:       int64(r.ID),
			JobName:  r.JobName,
			QueuedAt: r.QueuedAt,
			Status:   Status(r.Status),
			Trigger:  Trigger(r.Trigger),
			Error:    r.Error,
		}
		if r.StartedAt != nil {
			run.StartedAt = *r.StartedAt
		}
		if r.EndedAt != nil {
			run.EndedAt = *r.EndedAt
		}
		run.Args = decodeArgs(r.Args, run.ID)
		out = append(out, run)
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
