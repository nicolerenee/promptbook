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
// Runner.Start, or a missed-tick catch-up on restart;
// scheduled-fanout = a running job called EnqueueFromJob to chain a
// follow-up parameterized run.
const (
	TriggerScheduled       Trigger = "scheduled"
	TriggerManual          Trigger = "manual"
	TriggerStartup         Trigger = "startup"
	TriggerScheduledFanout Trigger = "scheduled-fanout"
)

// JobArgs is the parameter blob passed to a single Run. nil or empty
// for periodic / unparameterized jobs (e.g. refresh-encora,
// scan-incoming); populated when a fan-out job enqueues per-entity
// follow-ups (refresh-show-images {show_id: 498}). The map is
// JSON-marshalable — every value must round-trip through encoding/json
// because the blob is persisted in job_runs.args.
type JobArgs map[string]any

// GetInt64 returns the value under key as an int64, or 0 when missing
// or wrong-typed. Tolerates the float64 shape that encoding/json
// hands back for numeric values so a deserialized Run looks the same
// as one constructed in memory.
func (a JobArgs) GetInt64(key string) int64 {
	if a == nil {
		return 0
	}
	v, ok := a[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	default:
		return 0
	}
}

// GetString returns the value under key as a string, or "" when
// missing or wrong-typed.
func (a JobArgs) GetString(key string) string {
	if a == nil {
		return ""
	}
	v, ok := a[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// GetBool returns the value under key as a bool, or false when
// missing or wrong-typed.
func (a JobArgs) GetBool(key string) bool {
	if a == nil {
		return false
	}
	v, ok := a[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

// Job is the unit of work the Runner schedules and executes. The
// implementation is responsible for honoring ctx — the Runner cancels
// it on shutdown, but does not enforce a per-run timeout. Returning a
// non-nil error marks the Run as failed and persists err.Error() on
// the row; a panic is recovered with the same effect. args carries
// per-Run parameters; nil/empty for periodic and unparameterized
// jobs.
type Job interface {
	Name() string
	Run(ctx context.Context, args JobArgs) error
}

// Enqueuer is the slim interface a job depends on when it needs to
// chain follow-up work — typically refresh-encora fanning out per-show /
// per-recording / per-actor refresh jobs after the main sync finishes.
// The real *Runner satisfies this via EnqueueFromJob; tests can pass a
// stub that just records (name, args) calls.
//
// The interface lives here (next to Job) so jobs/builtin can depend on
// it without pulling in the whole *Runner type. Production wiring
// passes the runner itself; jobs that don't fan out leave the field
// nil and skip the call.
type Enqueuer interface {
	EnqueueFromJob(ctx context.Context, name string, args JobArgs) (*Run, error)
}

// JobDef registers a Job with the Runner along with its scheduling
// policy. Interval == 0 means "manual-only": the job is visible in
// ListScheduled, has no NextRun, and only fires when RunNow is
// called. OnStartup fires the job once when Runner.Start runs, ahead
// of the tick loop, with trigger=startup. DefaultArgs is the args
// blob passed to scheduled (interval-fired) and OnStartup runs;
// manual triggers via RunNow can override it. Leave DefaultArgs nil
// for plain unparameterized jobs.
type JobDef struct {
	Job         Job
	Interval    time.Duration
	OnStartup   bool
	DefaultArgs JobArgs
}

// Run is one invocation of a Job. ID is the autoincrement primary key
// from job_runs; QueuedAt is set when the Runner accepts the trigger;
// StartedAt is zero until a worker picks the run up; EndedAt is zero
// until Job.Run returns (or panics). Status / Error / Trigger / Args
// track the row's persisted columns one-to-one; Args is nil for
// runs that carry no parameters.
type Run struct {
	ID        int64
	JobName   string
	Args      JobArgs
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
