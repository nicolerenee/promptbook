package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// queueCapacity caps how many pending Runs the in-memory channel
// will buffer. 64 is comfortably larger than the active job count
// (currently two) plus a generous burst from manual triggers.
const queueCapacity = 64

// defaultWorkers is the worker-pool size used when Options.Workers is
// the zero value. Two matches Radarr's default and the design doc.
const defaultWorkers = 2

// tickInterval is how often the scheduler walks defs to decide what
// to enqueue. One second is fine-grained enough that 1m+ intervals
// fire on time and coarse enough that idle CPU stays near zero.
const tickInterval = time.Second

// Options configures a new Runner. DB and Logger are required; tests
// can pass an in-memory sqlite handle and zerolog.Nop().
type Options struct {
	DB      *sql.DB
	Logger  zerolog.Logger
	Workers int
	// Now is the clock the Runner consults. Defaults to time.Now;
	// tests inject a fake to make the scheduler tick deterministic.
	Now func() time.Time
}

// Runner owns the registered defs, the in-memory queue, the worker
// pool, and the persisted store. One Runner per process; the
// Register / RunNow / ListScheduled / ListRecent surface is the
// integration point the HTTP server uses.
type Runner struct {
	store   *Store
	logger  zerolog.Logger
	workers int
	now     func() time.Time

	// mu guards defs + state + active.
	mu    sync.RWMutex
	defs  map[string]JobDef
	state map[string]jobState
	// active dedups in-flight runs. Keyed on a composite of
	// (job name, canonical-JSON of args) so periodic runs and
	// manual runs with overrides don't conflict — only two callers
	// with identical (name, args) collide.
	active map[string]bool

	// queue carries Runs from the scheduler / RunNow into the worker
	// pool. Buffered (queueCapacity) so a brief enqueue burst doesn't
	// block the scheduler tick.
	queue chan *Run
}

// ErrUnknownJob is returned by RunNow / EnqueueFromJob when no Job
// is registered under the requested name.
var ErrUnknownJob = errors.New("jobs: unknown job")

// New constructs a Runner. Workers and Now default when zero.
func New(opts Options) *Runner {
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		store:   NewStore(opts.DB),
		logger:  opts.Logger,
		workers: workers,
		now:     now,
		defs:    make(map[string]JobDef),
		state:   make(map[string]jobState),
		active:  make(map[string]bool),
		queue:   make(chan *Run, queueCapacity),
	}
}

// Register adds def to the Runner. Must be called before Start —
// scheduling decisions consult the def map under a read lock so
// post-Start mutation is racy. Re-registering the same name returns
// an error rather than silently replacing.
func (r *Runner) Register(def JobDef) error {
	if def.Job == nil {
		return errors.New("jobs: JobDef.Job is required")
	}
	name := def.Job.Name()
	if name == "" {
		return errors.New("jobs: Job.Name() must be non-empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.defs[name]; exists {
		return fmt.Errorf("jobs: duplicate registration for %q", name)
	}
	r.defs[name] = def
	return nil
}

// Start launches the worker pool and the scheduler tick loop. It
// blocks until ctx is cancelled, at which point it drains the queue
// and returns. Returns ctx.Err() on shutdown.
//
// Order of operations on entry:
//  1. Hydrate r.state from the store so a restart can decide
//     whether to fire a missed-tick catch-up.
//  2. Start the worker goroutines.
//  3. Enqueue OnStartup jobs (and any catch-up runs whose interval
//     window has elapsed since last_ended_at).
//  4. Run the tick loop.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.hydrateState(ctx); err != nil {
		// Hydration failure is non-fatal: a missing job_state row is
		// expected on first boot, and a real I/O error here would
		// have been caught by storage.Open already. Log and proceed
		// with empty state — the scheduler will treat every job as
		// "never run" and fire on the next tick.
		r.logger.Warn().Err(err).Msg("jobs: state hydration failed; proceeding with empty state")
	}

	var wg sync.WaitGroup
	for i := range r.workers {
		wg.Add(1)
		go r.worker(ctx, &wg, i)
	}

	r.enqueueStartup(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(r.queue)
			wg.Wait()
			return ctx.Err()
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// hydrateState walks every registered def and pulls its persisted
// state into the in-memory map. Called once at Start so the
// scheduler's first tick can make an informed catch-up decision.
func (r *Runner) hydrateState(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.defs {
		st, err := r.store.loadState(ctx, name)
		if errors.Is(err, ErrNoState) {
			continue
		}
		if err != nil {
			return fmt.Errorf("hydrate %q: %w", name, err)
		}
		r.state[name] = st
	}
	return nil
}

// enqueueStartup fires every OnStartup-flagged def once at boot. A
// def whose interval window has already elapsed since last_ended_at
// also gets a startup run so a long downtime catches up cleanly.
// Both cases use trigger=startup so the queue/log makes the cause
// obvious.
func (r *Runner) enqueueStartup(ctx context.Context) {
	now := r.now()
	r.mu.RLock()
	defs := make([]JobDef, 0, len(r.defs))
	for _, d := range r.defs {
		defs = append(defs, d)
	}
	r.mu.RUnlock()

	for _, d := range defs {
		if r.shouldStartupFire(d, now) {
			if _, err := r.enqueue(ctx, d.Job.Name(), d.DefaultArgs, TriggerStartup); err != nil {
				r.logger.Error().
					Err(err).
					Str("job_name", d.Job.Name()).
					Msg("jobs: failed to enqueue startup run")
			}
		}
	}
}

// shouldStartupFire returns true when def should fire at startup —
// either because it's flagged OnStartup, or because its interval
// window has elapsed since last_ended_at (the catch-up case).
func (r *Runner) shouldStartupFire(def JobDef, now time.Time) bool {
	if def.OnStartup {
		return true
	}
	if def.Interval <= 0 {
		return false
	}
	r.mu.RLock()
	st, ok := r.state[def.Job.Name()]
	r.mu.RUnlock()
	if !ok || st.lastEndedAt == nil {
		return false
	}
	return now.Sub(*st.lastEndedAt) >= def.Interval
}

// tick is one scheduler pass: walk every def and enqueue any whose
// interval window has elapsed and aren't already active.
func (r *Runner) tick(ctx context.Context) {
	now := r.now()
	r.mu.RLock()
	defs := make([]JobDef, 0, len(r.defs))
	for _, d := range r.defs {
		defs = append(defs, d)
	}
	r.mu.RUnlock()

	for _, d := range defs {
		if d.Interval <= 0 {
			continue
		}
		name := d.Job.Name()
		key := dedupKey(name, d.DefaultArgs)
		r.mu.RLock()
		st, hasState := r.state[name]
		alreadyActive := r.active[key]
		r.mu.RUnlock()
		if alreadyActive {
			continue
		}
		if hasState && st.lastEndedAt != nil && now.Sub(*st.lastEndedAt) < d.Interval {
			continue
		}
		// Never-run job (no state): fire immediately so the first
		// observation lands on the very next tick rather than after
		// a full Interval delay.
		if _, err := r.enqueue(ctx, name, d.DefaultArgs, TriggerScheduled); err != nil {
			r.logger.Error().
				Err(err).
				Str("job_name", name).
				Msg("jobs: failed to enqueue scheduled run")
		}
	}
}

// RunNow enqueues an immediate manual run with the supplied args.
// Returns the freshly inserted Run (with ID populated) so the API can
// echo run_id back to the caller. Returns ErrUnknownJob when name
// isn't registered or an "already active" error when a run with the
// same (name, args) is already in flight.
func (r *Runner) RunNow(name string, args JobArgs) (*Run, error) {
	r.mu.RLock()
	_, ok := r.defs[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownJob, name)
	}
	return r.enqueue(context.Background(), name, args, TriggerManual)
}

// EnqueueFromJob queues a parameterized run from inside another
// running job. Returns ErrUnknownJob if no Job is registered under
// name; returns the enqueued *Run on success (the run is in 'queued'
// state). Used by long-running jobs (like refresh-encora) to fan out
// per-entity follow-ups without going through the public RunNow
// path. The resulting Run carries Trigger("scheduled-fanout") so the
// UI can distinguish chained work from user-clicked manual runs.
func (r *Runner) EnqueueFromJob(ctx context.Context, name string, args JobArgs) (*Run, error) {
	r.mu.RLock()
	_, ok := r.defs[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownJob, name)
	}
	return r.enqueue(ctx, name, args, TriggerScheduledFanout)
}

// dedupKey builds the (name, canonical-args) composite the active
// map uses to track in-flight runs. Empty / nil args produce the
// bare name + ":" prefix; populated args are encoded with stdlib
// json.Marshal which sorts map keys alphabetically since Go 1.12,
// so two equal-args calls always produce the same key.
func dedupKey(name string, args JobArgs) string {
	return name + ":" + canonicalArgs(args)
}

// canonicalArgs returns the canonical JSON encoding of args, or "" if
// args is nil/empty. encoding/json sorts map keys alphabetically, so
// two semantically-equal JobArgs always serialize to the same string.
// Marshal failures are treated as "no args" — every value type the
// public Get* helpers care about is JSON-safe by construction, so a
// failure here would be programmer error and we'd rather miss a
// dedup match than hard-fail an enqueue.
func canonicalArgs(args JobArgs) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// enqueue is the shared path that the scheduler, RunNow, and
// EnqueueFromJob all go through. It claims the active flag under the
// lock, persists the queued Run, then drops the *Run pointer onto
// the channel. The matching active-flag release happens in the
// worker after Job.Run returns; the InsertRun + active-set must
// therefore happen atomically (under r.mu) so a quick re-enqueue
// can't slip through.
func (r *Runner) enqueue(
	ctx context.Context, name string, args JobArgs, trig Trigger,
) (*Run, error) {
	key := dedupKey(name, args)
	r.mu.Lock()
	if r.active[key] {
		r.mu.Unlock()
		return nil, fmt.Errorf("jobs: %q is already active", name)
	}
	r.active[key] = true
	r.mu.Unlock()

	run := &Run{
		JobName:  name,
		Args:     args,
		QueuedAt: r.now(),
		Status:   StatusQueued,
		Trigger:  trig,
	}
	id, err := r.store.InsertRun(ctx, run)
	if err != nil {
		// Roll back the active flag so a persistent storage failure
		// doesn't leave the job permanently un-runnable.
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
		return nil, fmt.Errorf("persist queued run: %w", err)
	}
	run.ID = id

	// Send is non-blocking with the buffered queue; in the
	// pathological "queue full" case we surface the error so the
	// caller sees it rather than blocking the scheduler tick.
	select {
	case r.queue <- run:
		r.logger.Debug().
			Str("job_name", name).
			Int64("run_id", id).
			Str("trigger", string(trig)).
			Msg("jobs: enqueued run")
		return run, nil
	default:
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
		return nil, fmt.Errorf("jobs: queue full enqueueing %q", name)
	}
}

// worker pulls Runs from the queue and executes them. Wraps Job.Run
// in a recover so a panic in one job can't tear down the whole pool.
// Each run logs at start + end; the persisted job_runs row carries
// the ground truth.
func (r *Runner) worker(ctx context.Context, wg *sync.WaitGroup, idx int) {
	defer wg.Done()
	for run := range r.queue {
		r.executeRun(ctx, run, idx)
	}
}

// executeRun is one worker iteration extracted into its own
// function so the panic recover defers cleanly without leaking the
// surrounding loop.
func (r *Runner) executeRun(ctx context.Context, run *Run, workerIdx int) {
	def, ok := r.defForRun(run.JobName)
	if !ok {
		// The def disappeared between enqueue and dispatch
		// (impossible under current API, defensive guard).
		r.releaseActive(run.JobName, run.Args)
		return
	}

	startedAt := r.now()
	run.StartedAt = startedAt
	run.Status = StatusRunning
	if err := r.store.MarkStarted(ctx, run.ID, startedAt); err != nil {
		r.logger.Error().
			Err(err).
			Str("job_name", run.JobName).
			Int64("run_id", run.ID).
			Msg("jobs: failed to persist start; continuing")
	}

	logger := r.logger.With().
		Str("job_name", run.JobName).
		Int64("run_id", run.ID).
		Int("worker", workerIdx).
		Logger()
	logger.Info().Str("trigger", string(run.Trigger)).Msg("jobs: run started")

	status, errText := r.invoke(ctx, logger, def.Job, run.Args)

	endedAt := r.now()
	run.EndedAt = endedAt
	run.Status = status
	run.Error = errText
	if err := r.store.MarkEnded(
		ctx, run.ID, run.JobName, startedAt, endedAt, status, errText,
	); err != nil {
		logger.Error().Err(err).Msg("jobs: failed to persist end; state may be stale")
	}

	r.afterRun(run.JobName, run.Args, startedAt, endedAt, status)

	logger.Info().
		Str("status", string(status)).
		Dur("duration", endedAt.Sub(startedAt)).
		Msg("jobs: run finished")
}

// invoke runs job.Run with a panic recover. On panic, the stack is
// captured into the returned error string so an operator can find
// the offending line without grepping logs. Named returns are
// required so the deferred recover can rewrite the outcome on
// panic; the linter exception is the cheapest way to express
// "deferred mutation of return values".
//
//nolint:nonamedreturns // deferred recover must mutate return values
func (r *Runner) invoke(
	ctx context.Context, logger zerolog.Logger, job Job, args JobArgs,
) (status Status, errText string) {
	status = StatusSucceeded
	defer func() {
		if rec := recover(); rec != nil {
			stack := string(debug.Stack())
			logger.Error().
				Interface("panic", rec).
				Str("stack", stack).
				Msg("jobs: run panicked; treating as failed")
			status = StatusFailed
			errText = fmt.Sprintf("panic: %v", rec)
		}
	}()
	if err := job.Run(ctx, args); err != nil {
		status = StatusFailed
		errText = err.Error()
		logger.Error().Err(err).Msg("jobs: run returned error")
		return status, errText
	}
	return status, ""
}

// defForRun looks up the registered def for name under r.mu. Returns
// false when the name isn't registered (defensive — should be
// unreachable in production wiring).
func (r *Runner) defForRun(name string) (JobDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[name]
	return d, ok
}

// afterRun updates the in-memory state and clears the active flag
// once a run finishes. A separate helper keeps the locking
// localized. State is keyed on name only (not args) — the
// last-run summary aggregates across every (name, args) variant so
// the Scheduled view stays one-row-per-job.
func (r *Runner) afterRun(name string, args JobArgs, startedAt, endedAt time.Time, status Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	startedCopy := startedAt
	endedCopy := endedAt
	r.state[name] = jobState{
		lastStartedAt: &startedCopy,
		lastEndedAt:   &endedCopy,
		lastDuration:  endedAt.Sub(startedAt),
		lastStatus:    status,
	}
	delete(r.active, dedupKey(name, args))
}

// releaseActive clears the active flag for the (name, args) pair.
// Called on the "should never happen" def-missing branch in
// executeRun so a stuck flag can't accumulate.
func (r *Runner) releaseActive(name string, args JobArgs) {
	r.mu.Lock()
	delete(r.active, dedupKey(name, args))
	r.mu.Unlock()
}

// ListScheduled returns one ScheduledView per registered def. The
// rows are computed against r.state; freshly-registered jobs surface
// with all-nil pointers and an empty Status. NextRun is computed as
// last_ended_at + interval when both are known, else nil.
func (r *Runner) ListScheduled() []ScheduledView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ScheduledView, 0, len(r.defs))
	for name, def := range r.defs {
		view := ScheduledView{
			Name:     name,
			Interval: def.Interval,
		}
		if st, ok := r.state[name]; ok {
			view.LastStartedAt = st.lastStartedAt
			view.LastEndedAt = st.lastEndedAt
			view.LastDuration = st.lastDuration
			view.LastStatus = st.lastStatus
			if def.Interval > 0 && st.lastEndedAt != nil {
				next := st.lastEndedAt.Add(def.Interval)
				view.NextRun = &next
			}
		}
		out = append(out, view)
	}
	return out
}

// ListRecent forwards to the store; the API handler bounds limit.
func (r *Runner) ListRecent(ctx context.Context, limit int) ([]Run, error) {
	return r.store.ListRecent(ctx, limit)
}
