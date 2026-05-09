package jobs_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// fnJob is a tiny adapter so tests can register a closure as a Job
// without spelling out a struct each time.
type fnJob struct {
	name string
	fn   func(ctx context.Context) error
}

func (j *fnJob) Name() string                  { return j.name }
func (j *fnJob) Run(ctx context.Context) error { return j.fn(ctx) }

// counterJob counts invocations atomically and optionally blocks on
// a release channel so tests can assert dedup against a known
// in-flight run.
type counterJob struct {
	name    string
	calls   atomic.Int64
	release chan struct{}
	hold    bool
}

func (j *counterJob) Name() string { return j.name }
func (j *counterJob) Run(ctx context.Context) error {
	j.calls.Add(1)
	if j.hold {
		select {
		case <-j.release:
		case <-ctx.Done():
		}
	}
	return nil
}

func newRunner(t *testing.T) *jobs.Runner {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return jobs.New(jobs.Options{
		DB:      db,
		Logger:  zerolog.Nop(),
		Workers: 2,
	})
}

func TestRunner_RegisterListsScheduled(t *testing.T) {
	t.Parallel()
	r := newRunner(t)
	require.NoError(t, r.Register(jobs.JobDef{
		Job:      &fnJob{name: "alpha", fn: func(_ context.Context) error { return nil }},
		Interval: 5 * time.Minute,
	}))
	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "manual", fn: func(_ context.Context) error { return nil }},
	}))

	views := r.ListScheduled()
	require.Len(t, views, 2)

	names := map[string]jobs.ScheduledView{}
	for _, v := range views {
		names[v.Name] = v
	}
	assert.Equal(t, 5*time.Minute, names["alpha"].Interval)
	assert.Equal(t, time.Duration(0), names["manual"].Interval)
	assert.Nil(t, names["alpha"].LastEndedAt)
	assert.Nil(t, names["manual"].NextRun)
}

func TestRunner_DuplicateRegistration(t *testing.T) {
	t.Parallel()
	r := newRunner(t)
	job := &fnJob{name: "dup", fn: func(_ context.Context) error { return nil }}
	require.NoError(t, r.Register(jobs.JobDef{Job: job}))
	err := r.Register(jobs.JobDef{Job: job})
	require.Error(t, err)
}

func TestRunner_ScheduledIntervalFires(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	job := &counterJob{name: "tick"}
	require.NoError(t, r.Register(jobs.JobDef{
		Job:      job,
		Interval: 50 * time.Millisecond,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	// Wait long enough for at least three ticks (1s ticker means
	// each tick window is bounded by tickInterval, not Interval).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, job.calls.Load(), int64(3),
		"job should have fired at least 3 times")
}

func TestRunner_RunNow(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	job := &counterJob{name: "manual"}
	require.NoError(t, r.Register(jobs.JobDef{Job: job}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	run, err := r.RunNow("manual")
	require.NoError(t, err)
	assert.Positive(t, run.ID)
	assert.Equal(t, jobs.TriggerManual, run.Trigger)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Equal(t, int64(1), job.calls.Load())
}

func TestRunner_RunNowUnknown(t *testing.T) {
	t.Parallel()
	r := newRunner(t)
	_, err := r.RunNow("nope")
	require.Error(t, err)
}

func TestRunner_DedupSameName(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	release := make(chan struct{})
	job := &counterJob{name: "slow", release: release, hold: true}
	require.NoError(t, r.Register(jobs.JobDef{Job: job}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	run1, err := r.RunNow("slow")
	require.NoError(t, err)
	assert.Positive(t, run1.ID)

	// Wait for the worker to pick up run1 (calls == 1) so the active
	// flag is definitely set and the second RunNow exercises the
	// dedup branch rather than the "queue empty" race.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int64(1), job.calls.Load())

	_, err = r.RunNow("slow")
	require.Error(t, err, "second RunNow while first is in-flight should fail dedup")

	close(release)
}

func TestRunner_FailedJobRecorded(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "bad", fn: func(_ context.Context) error {
			return errors.New("kaboom")
		}},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	_, err := r.RunNow("bad")
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	var runs []jobs.Run
	for time.Now().Before(deadline) {
		runs, err = r.ListRecent(ctx, 5)
		require.NoError(t, err)
		if len(runs) == 1 && runs[0].Status == jobs.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Len(t, runs, 1)
	assert.Equal(t, jobs.StatusFailed, runs[0].Status)
	assert.Contains(t, runs[0].Error, "kaboom")
}

func TestRunner_PanicRecovered(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "panicky", fn: func(_ context.Context) error {
			panic("oh no")
		}},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	_, err := r.RunNow("panicky")
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	var runs []jobs.Run
	for time.Now().Before(deadline) {
		runs, err = r.ListRecent(ctx, 5)
		require.NoError(t, err)
		if len(runs) == 1 && runs[0].Status == jobs.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Len(t, runs, 1)
	assert.Equal(t, jobs.StatusFailed, runs[0].Status)
	assert.Contains(t, runs[0].Error, "panic")
}

func TestRunner_RestartHydratesState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "p.db")

	// First runner: register + run once + observe persisted state.
	db1, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	r1 := jobs.New(jobs.Options{DB: db1, Logger: zerolog.Nop(), Workers: 1})

	var (
		mu   sync.Mutex
		seen int
	)
	require.NoError(t, r1.Register(jobs.JobDef{
		Job: &fnJob{name: "persisted", fn: func(_ context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			seen++
			return nil
		}},
		Interval: time.Hour, // long enough that no second tick fires.
	}))

	ctx1, cancel1 := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_ = r1.Start(ctx1)
	}()

	_, err = r1.RunNow("persisted")
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := seen >= 1
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel1()
	<-doneCh
	_ = db1.Close()

	// Second runner against the same database: state should hydrate.
	db2, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
	r2 := jobs.New(jobs.Options{DB: db2, Logger: zerolog.Nop(), Workers: 1})
	require.NoError(t, r2.Register(jobs.JobDef{
		Job:      &fnJob{name: "persisted", fn: func(_ context.Context) error { return nil }},
		Interval: time.Hour,
	}))

	ctx2, cancel2 := context.WithCancel(context.Background())
	doneCh2 := make(chan struct{})
	go func() {
		defer close(doneCh2)
		_ = r2.Start(ctx2)
	}()
	// Give Start a beat to hydrate before we assert.
	time.Sleep(100 * time.Millisecond)

	views := r2.ListScheduled()
	require.Len(t, views, 1)
	assert.Equal(t, "persisted", views[0].Name)
	require.NotNil(t, views[0].LastEndedAt, "restart should hydrate last_ended_at from job_state")
	assert.Equal(t, jobs.StatusSucceeded, views[0].LastStatus)

	cancel2()
	<-doneCh2
}
