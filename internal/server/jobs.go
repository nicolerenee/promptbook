package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/jobs"
)

// Job queue listing tunables. The JSON shape is intentionally
// JS-friendly: durations as integer milliseconds, every nullable
// timestamp as a *string in RFC3339 so the Mithril page can render
// them with relativeTime() unmodified.
const (
	defaultJobQueueLimit = 100
	maxJobQueueLimit     = 500
)

// scheduledJobJSON is one row in /api/v1/jobs/scheduled. Pointers are
// nil when the runner has never observed the corresponding state, so
// the UI can render "—" for never-run jobs without sentinel checks.
type scheduledJobJSON struct {
	Name           string  `json:"name"`
	IntervalMs     int64   `json:"interval_ms"`
	LastStartedAt  *string `json:"last_started_at"`
	LastEndedAt    *string `json:"last_ended_at"`
	LastDurationMs int64   `json:"last_duration_ms"`
	LastStatus     string  `json:"last_status"`
	NextRun        *string `json:"next_run"`
}

// jobRunJSON is one row in /api/v1/jobs/queue. DurationMs is computed
// server-side so the UI's mm:ss formatter has a stable value to work
// off of (clients otherwise diverge on Date.parse semantics for the
// "running" case).
type jobRunJSON struct {
	ID         int64   `json:"id"`
	JobName    string  `json:"job_name"`
	QueuedAt   string  `json:"queued_at"`
	StartedAt  *string `json:"started_at"`
	EndedAt    *string `json:"ended_at"`
	Status     string  `json:"status"`
	Error      string  `json:"error"`
	Trigger    string  `json:"trigger"`
	DurationMs int64   `json:"duration_ms"`
}

// handleListScheduledJobs renders the runner's ListScheduled output.
// Returns 503 when no runner is wired so the page can show a
// graceful "not configured" state without dropping a 500.
func (s *Server) handleListScheduledJobs(c echo.Context) error {
	if s.jobRunner == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "jobs not configured")
	}
	views := s.jobRunner.ListScheduled()
	out := make([]scheduledJobJSON, 0, len(views))
	for _, v := range views {
		row := scheduledJobJSON{
			Name:           v.Name,
			IntervalMs:     v.Interval.Milliseconds(),
			LastDurationMs: v.LastDuration.Milliseconds(),
			LastStatus:     string(v.LastStatus),
		}
		if v.LastStartedAt != nil {
			t := v.LastStartedAt.UTC().Format(time.RFC3339)
			row.LastStartedAt = &t
		}
		if v.LastEndedAt != nil {
			t := v.LastEndedAt.UTC().Format(time.RFC3339)
			row.LastEndedAt = &t
		}
		if v.NextRun != nil {
			t := v.NextRun.UTC().Format(time.RFC3339)
			row.NextRun = &t
		}
		out = append(out, row)
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: out})
}

// handleListJobQueue surfaces recent runs. ?limit defaults to
// defaultJobQueueLimit and is capped at maxJobQueueLimit so a
// runaway client can't sweep the whole table.
func (s *Server) handleListJobQueue(c echo.Context) error {
	if s.jobRunner == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "jobs not configured")
	}
	limit := paramInt(c, "limit", defaultJobQueueLimit)
	if limit <= 0 {
		limit = defaultJobQueueLimit
	}
	limit = min(limit, maxJobQueueLimit)
	runs, err := s.jobRunner.ListRecent(c.Request().Context(), limit)
	if err != nil {
		return err
	}
	out := make([]jobRunJSON, 0, len(runs))
	for _, r := range runs {
		out = append(out, runToJSON(r))
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: out})
}

// handleRunJob is the manual-trigger endpoint. 404 when the name is
// unknown, 409 when a run is already active for that job (dedup
// contract), 200 with the new run_id otherwise.
func (s *Server) handleRunJob(c echo.Context) error {
	if s.jobRunner == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "jobs not configured")
	}
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	run, err := s.jobRunner.RunNow(name)
	if err != nil {
		if strings.Contains(err.Error(), "unknown job") {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		if strings.Contains(err.Error(), "already active") {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"run_id": run.ID})
}

// runToJSON converts a jobs.Run into the JSON shape the UI expects.
// duration_ms is end-started when both are present; for in-flight
// runs we surface the elapsed-since-started so the queue table can
// show a live counter without an extra clock import.
func runToJSON(r jobs.Run) jobRunJSON {
	out := jobRunJSON{
		ID:       r.ID,
		JobName:  r.JobName,
		QueuedAt: r.QueuedAt.UTC().Format(time.RFC3339),
		Status:   string(r.Status),
		Error:    r.Error,
		Trigger:  string(r.Trigger),
	}
	if !r.StartedAt.IsZero() {
		s := r.StartedAt.UTC().Format(time.RFC3339)
		out.StartedAt = &s
	}
	if !r.EndedAt.IsZero() {
		s := r.EndedAt.UTC().Format(time.RFC3339)
		out.EndedAt = &s
		if !r.StartedAt.IsZero() {
			out.DurationMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
		}
	}
	return out
}
