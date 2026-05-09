package server

// refresh_images.go — per-entity "Refresh from upstream" endpoints
// the picker modal footer fires. Each handler enqueues the matching
// background job (refresh-recording-images / refresh-show-images) and
// returns the run id so the SPA can poll for completion if it cares.
//
// These are thin wrappers around jobRunner.RunNow. The real work
// (StageMedia + Encora calls, slot writes, render) happens inside the
// job; the endpoint exists so the picker can fire it without going
// through the generic /api/v1/jobs/scheduled/:name/run surface, which
// the queue page also exposes — the per-entity endpoints validate the
// id, attach it as the right job arg, and short-circuit obvious
// errors (404 unknown id, 503 no runner) before the queue gets a row.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/jobs"
)

// refreshImagesRequest is the optional JSON body for the per-entity
// refresh endpoints. Force=true tells the underlying job to re-fetch
// upstream even when the slot file is already on disk.
type refreshImagesRequest struct {
	Force bool `json:"force"`
}

// refreshImagesResponse is the JSON envelope. RunID is the freshly
// enqueued run so the SPA can hit /api/v1/jobs/queue and watch for
// completion.
type refreshImagesResponse struct {
	RunID int64 `json:"run_id"`
}

// jobNameRefreshRecordingImages mirrors the constant in
// internal/jobs/builtin/refresh_recording_images.go. Duplicated here
// (vs imported) so the server package doesn't pull builtin into its
// dependency graph — the runner only knows job names as strings.
const (
	jobNameRefreshRecordingImages = "refresh-recording-images"
	jobNameRefreshShowImages      = "refresh-show-images"
)

// requireJobRunner returns a 503 echo error when no jobs.Runner is
// wired. Mirrors the guards on the existing /api/v1/jobs/* handlers.
func (s *Server) requireJobRunner() error {
	if s.jobRunner == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "jobs not configured")
	}
	return nil
}

// parseRefreshImagesBody decodes the optional JSON body. An empty
// body means force=false, which is the common path; Bind only runs
// when ContentLength > 0 so a missing payload doesn't trip a 400.
func parseRefreshImagesBody(c echo.Context) (refreshImagesRequest, error) {
	var req refreshImagesRequest
	if c.Request().ContentLength <= 0 {
		return req, nil
	}
	if err := c.Bind(&req); err != nil {
		return req, echo.NewHTTPError(http.StatusBadRequest,
			"decode body: "+err.Error())
	}
	return req, nil
}

// enqueueRefreshJob is the shared "fire the named refresh job for
// (idKey, id) with optional force" path used by both
// handleRefreshRecordingImages and handleRefreshShowImages. Maps
// runner errors to 404 / 409 / 500 so the SPA can branch on status
// without parsing the body.
func (s *Server) enqueueRefreshJob(
	jobName, idKey string, id int64, force bool,
) (int64, error) {
	args := jobs.JobArgs{idKey: id}
	if force {
		args["force"] = true
	}
	run, err := s.jobRunner.RunNow(jobName, args)
	if err != nil {
		if errors.Is(err, jobs.ErrUnknownJob) {
			return 0, echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		if strings.Contains(err.Error(), "already active") {
			return 0, echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return 0, err
	}
	return run.ID, nil
}

// handleRefreshRecordingImages handles
// POST /api/v1/recordings/:id/refresh-images. Optional body:
// {"force": true}. Enqueues refresh-recording-images with the parsed
// id and returns the run id.
func (s *Server) handleRefreshRecordingImages(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if runnerErr := s.requireJobRunner(); runnerErr != nil {
		return runnerErr
	}
	if existsErr := s.recordingExists(c, id); existsErr != nil {
		return existsErr
	}
	req, parseErr := parseRefreshImagesBody(c)
	if parseErr != nil {
		return parseErr
	}
	runID, enqueueErr := s.enqueueRefreshJob(
		jobNameRefreshRecordingImages, "recording_id", id, req.Force,
	)
	if enqueueErr != nil {
		return enqueueErr
	}
	return c.JSON(http.StatusOK, refreshImagesResponse{RunID: runID})
}

// handleRefreshShowImages handles
// POST /api/v1/shows/:id/refresh-images. Optional body:
// {"force": true}. Enqueues refresh-show-images with the parsed id
// and returns the run id.
func (s *Server) handleRefreshShowImages(c echo.Context) error {
	id, err := parseShowIDParam(c)
	if err != nil {
		return err
	}
	if runnerErr := s.requireJobRunner(); runnerErr != nil {
		return runnerErr
	}
	if existsErr := s.showExists(c.Request().Context(), id); existsErr != nil {
		return existsErr
	}
	req, parseErr := parseRefreshImagesBody(c)
	if parseErr != nil {
		return parseErr
	}
	runID, enqueueErr := s.enqueueRefreshJob(
		jobNameRefreshShowImages, "show_id", id, req.Force,
	)
	if enqueueErr != nil {
		return enqueueErr
	}
	return c.JSON(http.StatusOK, refreshImagesResponse{RunID: runID})
}
