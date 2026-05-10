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
	fn   func(ctx context.Context, args jobs.JobArgs) error
}

func (j *fnJob) Name() string { return j.name }
func (j *fnJob) Run(ctx context.Context, args jobs.JobArgs) error {
	return j.fn(ctx, args)
}

// counterJob counts invocations atomically and optionally blocks on
// a release channel so tests can assert dedup against a known
// in-flight run. lastArgs is captured for tests that assert the
// args blob round-trips through the runner.
type counterJob struct {
	name     string
	calls    atomic.Int64
	release  chan struct{}
	hold     bool
	mu       sync.Mutex
	lastArgs jobs.JobArgs
	allArgs  []jobs.JobArgs
}

func (j *counterJob) Name() string { return j.name }
func (j *counterJob) Run(ctx context.Context, args jobs.JobArgs) error {
	j.calls.Add(1)
	j.mu.Lock()
	j.lastArgs = args
	j.allArgs = append(j.allArgs, args)
	j.mu.Unlock()
	if j.hold {
		select {
		case <-j.release:
		case <-ctx.Done():
		}
	}
	return nil
}

func (j *counterJob) buttonshotArgs() (jobs.JobArgs, []jobs.JobArgs) {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := make([]jobs.JobArgs, len(j.allArgs))
	copy(cp, j.allArgs)
	return j.lastArgs, cp
}

func newRunner(t *testing.T) *jobs.Runner {
	t.Helper()
	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
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
		Job:      &fnJob{name: "alpha", fn: func(_ context.Context, _ jobs.JobArgs) error { return nil }},
		Interval: 5 * time.Minute,
	}))
	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "manual", fn: func(_ context.Context, _ jobs.JobArgs) error { return nil }},
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
	job := &fnJob{name: "dup", fn: func(_ context.Context, _ jobs.JobArgs) error { return nil }}
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

	run, err := r.RunNow("manual", nil)
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
	_, err := r.RunNow("nope", nil)
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

	run1, err := r.RunNow("slow", nil)
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

	_, err = r.RunNow("slow", nil)
	require.Error(t, err, "second RunNow while first is in-flight should fail dedup")

	close(release)
}

func TestRunner_FailedJobRecorded(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "bad", fn: func(_ context.Context, _ jobs.JobArgs) error {
			return errors.New("kaboom")
		}},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	_, err := r.RunNow("bad", nil)
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
		Job: &fnJob{name: "panicky", fn: func(_ context.Context, _ jobs.JobArgs) error {
			panic("oh no")
		}},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	_, err := r.RunNow("panicky", nil)
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
	sqlDB1, db1, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	r1 := jobs.New(jobs.Options{DB: db1, Logger: zerolog.Nop(), Workers: 1})

	var (
		mu   sync.Mutex
		seen int
	)
	require.NoError(t, r1.Register(jobs.JobDef{
		Job: &fnJob{name: "persisted", fn: func(_ context.Context, _ jobs.JobArgs) error {
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

	_, err = r1.RunNow("persisted", nil)
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
	_ = sqlDB1.Close()

	// Second runner against the same database: state should hydrate.
	sqlDB2, db2, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB2.Close() })
	r2 := jobs.New(jobs.Options{DB: db2, Logger: zerolog.Nop(), Workers: 1})
	require.NoError(t, r2.Register(jobs.JobDef{
		Job:      &fnJob{name: "persisted", fn: func(_ context.Context, _ jobs.JobArgs) error { return nil }},
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

// TestRunNow_WithArgs verifies the args blob is recorded on the
// resulting Run, surfaces in ListRecent, and reaches Job.Run intact.
func TestRunNow_WithArgs(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	job := &counterJob{name: "with-args"}
	require.NoError(t, r.Register(jobs.JobDef{Job: job}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	args := jobs.JobArgs{"show_id": int64(498), "force": true}
	run, err := r.RunNow("with-args", args)
	require.NoError(t, err)
	assert.Positive(t, run.ID)
	assert.Equal(t, args, run.Args)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, int64(1), job.calls.Load())

	last, _ := job.buttonshotArgs()
	require.NotNil(t, last)
	assert.Equal(t, int64(498), last.GetInt64("show_id"))
	assert.True(t, last.GetBool("force"))

	runs, err := r.ListRecent(ctx, 5)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.NotNil(t, runs[0].Args)
	assert.Equal(t, int64(498), runs[0].Args.GetInt64("show_id"))
	assert.True(t, runs[0].Args.GetBool("force"))
}

// TestDedupByArgs verifies the (name, args) composite key: two calls
// with the same args dedup, two calls with different args both run.
func TestDedupByArgs(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	release := make(chan struct{})
	job := &counterJob{name: "fanout", release: release, hold: true}
	require.NoError(t, r.Register(jobs.JobDef{Job: job}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	// Same args twice → second blocks on dedup.
	argsA := jobs.JobArgs{"show_id": int64(1)}
	_, err := r.RunNow("fanout", argsA)
	require.NoError(t, err)

	// Wait for the worker to pick up the first run so the active
	// flag is definitely set.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int64(1), job.calls.Load())

	_, err = r.RunNow("fanout", jobs.JobArgs{"show_id": int64(1)})
	require.Error(t, err, "duplicate (name, args) should dedup")

	// Different args → both run concurrently (workers=2 in newRunner).
	_, err = r.RunNow("fanout", jobs.JobArgs{"show_id": int64(2)})
	require.NoError(t, err, "different args should bypass dedup")

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, job.calls.Load(), int64(2),
		"different-args run should not be blocked by dedup")

	close(release)
}

// TestEnqueueFromJob verifies a parent job can chain a child run via
// EnqueueFromJob, and the child carries Trigger("scheduled-fanout")
// and the supplied args.
func TestEnqueueFromJob(t *testing.T) {
	t.Parallel()
	r := newRunner(t)

	childArgs := jobs.JobArgs{"recording_id": int64(42), "force": true}
	childRan := make(chan jobs.JobArgs, 1)
	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "child", fn: func(_ context.Context, args jobs.JobArgs) error {
			childRan <- args
			return nil
		}},
	}))

	require.NoError(t, r.Register(jobs.JobDef{
		Job: &fnJob{name: "parent", fn: func(ctx context.Context, _ jobs.JobArgs) error {
			_, err := r.EnqueueFromJob(ctx, "child", childArgs)
			return err
		}},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Start(ctx) }()
	defer cancel()

	_, err := r.RunNow("parent", nil)
	require.NoError(t, err)

	select {
	case got := <-childRan:
		require.NotNil(t, got)
		assert.Equal(t, int64(42), got.GetInt64("recording_id"))
		assert.True(t, got.GetBool("force"))
	case <-time.After(2 * time.Second):
		t.Fatal("child job never ran via EnqueueFromJob")
	}

	// Allow the runs to drain so ListRecent has them both.
	deadline := time.Now().Add(2 * time.Second)
	var runs []jobs.Run
	for time.Now().Before(deadline) {
		runs, err = r.ListRecent(ctx, 5)
		require.NoError(t, err)
		if len(runs) >= 2 {
			done := true
			for _, run := range runs {
				if run.Status == jobs.StatusQueued || run.Status == jobs.StatusRunning {
					done = false
					break
				}
			}
			if done {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, len(runs), 2)

	var sawFanout bool
	for _, run := range runs {
		if run.JobName == "child" {
			assert.Equal(t, jobs.TriggerScheduledFanout, run.Trigger)
			sawFanout = true
		}
	}
	assert.True(t, sawFanout, "child run should carry scheduled-fanout trigger")
}

// TestEnqueueFromJob_UnknownJob verifies the ErrUnknownJob sentinel
// is wrapped so callers (and the HTTP handler) can branch on it.
func TestEnqueueFromJob_UnknownJob(t *testing.T) {
	t.Parallel()
	r := newRunner(t)
	_, err := r.EnqueueFromJob(context.Background(), "nope", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, jobs.ErrUnknownJob)
}

// TestRestartState_PreservesArgs verifies a Run's args round-trip
// through the database: register + fire with args + close + reopen +
// ListRecent shows the args intact.
func TestRestartState_PreservesArgs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "p.db")

	sqlDB1, db1, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	r1 := jobs.New(jobs.Options{DB: db1, Logger: zerolog.Nop(), Workers: 1})

	job := &counterJob{name: "args-persisted"}
	require.NoError(t, r1.Register(jobs.JobDef{Job: job, Interval: time.Hour}))

	ctx1, cancel1 := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_ = r1.Start(ctx1)
	}()

	args := jobs.JobArgs{"actor_id": int64(1234), "label": "headshot"}
	_, err = r1.RunNow("args-persisted", args)
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job.calls.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel1()
	<-doneCh
	_ = sqlDB1.Close()

	// Reopen against the same database; ListRecent is on the store,
	// so we don't need to start the runner.
	sqlDB2, db2, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB2.Close() })
	r2 := jobs.New(jobs.Options{DB: db2, Logger: zerolog.Nop(), Workers: 1})

	runs, err := r2.ListRecent(ctx, 5)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.NotNil(t, runs[0].Args)
	assert.Equal(t, int64(1234), runs[0].Args.GetInt64("actor_id"))
	assert.Equal(t, "headshot", runs[0].Args.GetString("label"))
}

// TestArgsHelpers verifies the GetInt64 / GetString / GetBool helpers
// against the value types JobArgs sees in practice — including the
// float64 shape json.Unmarshal hands back for numbers.
func TestArgsHelpers(t *testing.T) {
	t.Parallel()

	args := jobs.JobArgs{
		"int_native":   int(7),
		"int64_native": int64(8),
		"float_json":   float64(9), // shape after json.Unmarshal.
		"name":         "alpha",
		"truthy":       true,
		"falsy":        false,
		"misc_string":  "not-a-number",
	}

	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{name: "GetInt64 native int", check: func(t *testing.T) {
			assert.Equal(t, int64(7), args.GetInt64("int_native"))
		}},
		{name: "GetInt64 native int64", check: func(t *testing.T) {
			assert.Equal(t, int64(8), args.GetInt64("int64_native"))
		}},
		{name: "GetInt64 from json float64", check: func(t *testing.T) {
			assert.Equal(t, int64(9), args.GetInt64("float_json"))
		}},
		{name: "GetInt64 missing key", check: func(t *testing.T) {
			assert.Equal(t, int64(0), args.GetInt64("missing"))
		}},
		{name: "GetInt64 wrong type", check: func(t *testing.T) {
			assert.Equal(t, int64(0), args.GetInt64("name"))
		}},
		{name: "GetString found", check: func(t *testing.T) {
			assert.Equal(t, "alpha", args.GetString("name"))
		}},
		{name: "GetString missing", check: func(t *testing.T) {
			assert.Empty(t, args.GetString("missing"))
		}},
		{name: "GetString wrong type", check: func(t *testing.T) {
			assert.Empty(t, args.GetString("truthy"))
		}},
		{name: "GetBool true", check: func(t *testing.T) {
			assert.True(t, args.GetBool("truthy"))
		}},
		{name: "GetBool false", check: func(t *testing.T) {
			assert.False(t, args.GetBool("falsy"))
		}},
		{name: "GetBool missing", check: func(t *testing.T) {
			assert.False(t, args.GetBool("missing"))
		}},
		{name: "GetBool wrong type", check: func(t *testing.T) {
			assert.False(t, args.GetBool("name"))
		}},
		{name: "nil JobArgs returns zero values", check: func(t *testing.T) {
			var nilArgs jobs.JobArgs
			assert.Equal(t, int64(0), nilArgs.GetInt64("anything"))
			assert.Empty(t, nilArgs.GetString("anything"))
			assert.False(t, nilArgs.GetBool("anything"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.check(t)
		})
	}
}
