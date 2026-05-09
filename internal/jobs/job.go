// Package jobs provides a Radarr-style scheduled-jobs framework: an
// in-process scheduler ticks registered Job definitions on their
// configured interval, drops Run records onto an in-memory channel
// queue, and a small worker pool drains the queue. Every state
// transition (queued, started, ended) is persisted so a crash leaves
// an honest record on disk; on restart the Runner replays job_state
// to populate its in-memory bookkeeping. The shape mirrors a NATS-
// style pub/sub but lives entirely in one process — swapping in a
// real broker later requires no changes to Job.Run.
package jobs

import (
	"context"
	"time"
)

// Status is the lifecycle stage of a single Run.
type Status string

// Status values mirror the four terminal/transient states a Run can
// occupy. Strings, not ints, because the values land in SQLite via
// status TEXT and surface in the API JSON unchanged.
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Trigger labels how a Run was created. Used in logs, the UI, and the
// job_runs.trigger column so an operator can tell a user-clicked
// manual run apart from an interval tick or a startup catch-up.
type Trigger string

// Trigger values: scheduled = the ticker fired; manual = a UI/API
// caller invoked RunNow; startup = an OnStartup job fired once at
// Runner.Start, or a missed-tick catch-up on restart.
const (
	TriggerScheduled Trigger = "scheduled"
	TriggerManual    Trigger = "manual"
	TriggerStartup   Trigger = "startup"
)

// Job is the unit of work the Runner schedules and executes. The
// implementation is responsible for honoring ctx — the Runner cancels
// it on shutdown, but does not enforce a per-run timeout. Returning a
// non-nil error marks the Run as failed and persists err.Error() on
// the row; a panic is recovered with the same effect.
type Job interface {
	Name() string
	Run(ctx context.Context) error
}

// JobDef registers a Job with the Runner along with its scheduling
// policy. Interval == 0 means "manual-only": the job is visible in
// ListScheduled, has no NextRun, and only fires when RunNow is
// called. OnStartup fires the job once when Runner.Start runs, ahead
// of the tick loop, with trigger=startup.
type JobDef struct {
	Job       Job
	Interval  time.Duration
	OnStartup bool
}

// Run is one invocation of a Job. ID is the autoincrement primary key
// from job_runs; QueuedAt is set when the Runner accepts the trigger;
// StartedAt is zero until a worker picks the run up; EndedAt is zero
// until Job.Run returns (or panics). Status / Error / Trigger track
// the row's persisted columns one-to-one.
type Run struct {
	ID        int64
	JobName   string
	QueuedAt  time.Time
	StartedAt time.Time
	EndedAt   time.Time
	Status    Status
	Error     string
	Trigger   Trigger
}

// ScheduledView is the per-job summary the API surfaces to the UI.
// The pointers are nil when the Runner has never observed the
// corresponding state — e.g. a freshly registered job has no last
// run, so LastStartedAt + LastEndedAt are both nil and LastStatus is
// the empty Status. NextRun is nil for manual-only jobs (Interval ==
// 0) and for jobs whose interval window doesn't have a meaningful
// projection yet (i.e. never run).
type ScheduledView struct {
	Name          string
	Interval      time.Duration
	LastStartedAt *time.Time
	LastEndedAt   *time.Time
	LastDuration  time.Duration
	LastStatus    Status
	NextRun       *time.Time
}
