package jobs_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// openTestStore stands up a fresh sqlite database in a temp dir and
// wraps it in a jobs.Store. Returned to every test that needs raw
// store access.
func openTestStore(t *testing.T) *jobs.Store {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return jobs.NewStore(db)
}

func TestStore_RunLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openTestStore(t)

	queuedAt := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	startedAt := queuedAt.Add(50 * time.Millisecond)
	endedAt := startedAt.Add(150 * time.Millisecond)

	r := &jobs.Run{
		JobName:  "demo",
		QueuedAt: queuedAt,
		Status:   jobs.StatusQueued,
		Trigger:  jobs.TriggerManual,
	}
	id, err := store.InsertRun(ctx, r)
	require.NoError(t, err)
	assert.Positive(t, id)

	require.NoError(t, store.MarkStarted(ctx, id, startedAt))
	require.NoError(t, store.MarkEnded(ctx, id, "demo", startedAt, endedAt, jobs.StatusSucceeded, ""))

	runs, err := store.ListRecent(ctx, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	got := runs[0]
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "demo", got.JobName)
	assert.Equal(t, jobs.StatusSucceeded, got.Status)
	assert.Equal(t, jobs.TriggerManual, got.Trigger)
	assert.WithinDuration(t, startedAt, got.StartedAt, time.Second)
	assert.WithinDuration(t, endedAt, got.EndedAt, time.Second)
	assert.Empty(t, got.Error)
}

// TestStore_LoadStateNoRow verifies the loadState behavior via the
// public ListScheduled path on the Runner: a freshly registered job
// with no persisted history surfaces with all-nil pointers.
func TestStore_LoadStateNoRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	r := jobs.New(jobs.Options{DB: db, Workers: 1})
	require.NoError(t, r.Register(jobs.JobDef{
		Job: stubJob{name: "missing"},
	}))
	views := r.ListScheduled()
	require.Len(t, views, 1)
	assert.Equal(t, "missing", views[0].Name)
	assert.Nil(t, views[0].LastEndedAt)
	assert.Empty(t, string(views[0].LastStatus))
}

// stubJob is a no-op Job used to sanity-check ScheduledView for a
// freshly registered name.
type stubJob struct{ name string }

func (s stubJob) Name() string                              { return s.name }
func (stubJob) Run(_ context.Context, _ jobs.JobArgs) error { return nil }

func TestStore_MarkEndedUpsertsState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openTestStore(t)

	queuedAt := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	startedAt := queuedAt.Add(time.Second)
	endedAt := startedAt.Add(2 * time.Second)

	r := &jobs.Run{
		JobName: "demo", QueuedAt: queuedAt,
		Status: jobs.StatusQueued, Trigger: jobs.TriggerScheduled,
	}
	id, err := store.InsertRun(ctx, r)
	require.NoError(t, err)
	require.NoError(t, store.MarkStarted(ctx, id, startedAt))
	require.NoError(t, store.MarkEnded(ctx, id, "demo", startedAt, endedAt, jobs.StatusFailed, "boom"))

	runs, err := store.ListRecent(ctx, 5)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "boom", runs[0].Error)
	assert.Equal(t, jobs.StatusFailed, runs[0].Status)
}
