package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// EncoraWriteClient is the slice of *encora.Client this package needs to
// push proposed changes back to Encora. Surfacing it as an interface lets
// tests substitute a stub without spinning up an httptest server. The real
// *encora.Client satisfies this interface; out-of-scope mutating endpoints
// (RemoveFromCollection, RemoveFromWants, AddToWants) deliberately stay
// off the surface so we can't push destructive changes by accident.
type EncoraWriteClient interface {
	AddToCollection(ctx context.Context, id int64) (encora.RateLimitInfo, error)
	UpdateCollectionFormat(
		ctx context.Context, id int64, format string,
	) (encora.RateLimitInfo, error)
}

// Retry-After honor bounds. A 429 with no Retry-After is rare in
// practice but possible — fall back to 30s so we don't immediately re-
// fire and trigger a cascade of 429s. Cap at 90s so a misbehaving
// upstream can't park the whole apply batch on a multi-minute pause.
const (
	defaultRetryAfter = 30 * time.Second
	maxRetryAfter     = 90 * time.Second
)

// ApplyAction is one user-selected push to Encora. Type identifies which
// mismatch is being resolved; NewFormat carries the proposed format string
// for FormatMismatch and is empty otherwise.
type ApplyAction struct {
	Type        MismatchType `json:"type"`
	RecordingID int64        `json:"recording_id"`
	NewFormat   string       `json:"new_format"`
}

// ApplyResult is the per-action outcome reported back to the caller.
// HTTPStatus is 0 when the call never reached Encora (validation failure
// or short-circuit) and the upstream HTTP status equivalent otherwise.
// Error is empty when OK is true.
type ApplyResult struct {
	Action     ApplyAction `json:"action"`
	OK         bool        `json:"ok"`
	Error      string      `json:"error,omitempty"`
	HTTPStatus int         `json:"http_status"`
}

// actionKey is the (type, recording_id) tuple used as the map key for
// validating submitted actions against the live mismatch oracle.
type actionKey struct {
	Type MismatchType
	ID   int64
}

// validationResult bundles the outputs of buildValidationSet — a set of
// keys the apply pass will accept, plus the LocalFormat oracle for
// FormatMismatch rows so we can reject tampered NewFormat values.
type validationResult struct {
	allowed      map[actionKey]struct{}
	localFormats map[int64]string
}

// buildValidationSet projects the live mismatch report into the lookup
// shapes the apply handler needs. Built once per request so a malicious
// or stale form can't slip a stale (or fabricated) action past the
// EncoraWriteClient surface.
func buildValidationSet(ctx context.Context, client *ent.Client) (validationResult, error) {
	items, err := loadMismatches(ctx, client, nil)
	if err != nil {
		return validationResult{}, fmt.Errorf("load mismatches for apply validation: %w", err)
	}
	v := validationResult{
		allowed:      make(map[actionKey]struct{}, len(items)),
		localFormats: make(map[int64]string),
	}
	for _, item := range items {
		v.allowed[actionKey{Type: item.Type, ID: item.RecordingID}] = struct{}{}
		if item.Type == MismatchTypeFormatMismatch {
			v.localFormats[item.RecordingID] = item.LocalFormat
		}
	}
	return v, nil
}

// validateAction returns the ApplyResult to use when the submitted
// action either no longer matches the live mismatch report (state
// changed under the user's feet) or — for FormatMismatch — when the
// submitted NewFormat doesn't match the recording's current LocalFormat.
// ok=true means the action passed validation and the caller should
// proceed to applyOne.
func (v validationResult) validate(action ApplyAction) (ApplyResult, bool) {
	if _, found := v.allowed[actionKey{Type: action.Type, ID: action.RecordingID}]; !found {
		return ApplyResult{
			Action: action,
			Error:  "action no longer applies — recording state has changed",
		}, false
	}
	if action.Type == MismatchTypeFormatMismatch {
		want := v.localFormats[action.RecordingID]
		if action.NewFormat != want {
			return ApplyResult{
				Action: action,
				Error:  "format mismatch: submitted format doesn't match current local format",
			}, false
		}
	}
	return ApplyResult{}, true
}

// applyOne dispatches a single ApplyAction to the right Encora write
// endpoint, surfacing per-error detail in ApplyResult. Successful pushes
// are recorded as HistoryKindEncoraPush events so the audit trail
// matches what changed upstream. Failures intentionally do NOT write a
// history row — the server's history log is the user-visible record of
// "things that took effect", not "things we tried".
//
// sleepBudget is an out-parameter the batch driver inspects after each
// call. When applyOne hits ErrRateLimited it populates *sleepBudget
// with the parsed Retry-After (clamped to [defaultRetryAfter,
// maxRetryAfter]) so the next iteration can sleep before issuing the
// next request — a single 429 doesn't poison the whole batch with a
// cascade of immediate-retry 429s. A non-rate-limited error leaves the
// budget at zero.
//
// Removal flows (RemoveFromCollection, RemoveFromWants) are intentionally
// out of scope here — destructive pushes need a dedicated confirmation UI
// and history-event taxonomy.
func applyOne(
	ctx context.Context,
	client EncoraWriteClient,
	db *ent.Client,
	action ApplyAction,
	sleepBudget *time.Duration,
) ApplyResult {
	res := ApplyResult{Action: action}

	switch action.Type {
	case MismatchTypeAddToCollection:
		rl, err := client.AddToCollection(ctx, action.RecordingID)
		if err != nil {
			res.Error, res.HTTPStatus = describeEncoraError(err)
			recordRateLimitBudget(err, rl, sleepBudget)
			return res
		}
		recordEncoraPush(ctx, db,
			fmt.Sprintf("Added recording %d to Encora collection", action.RecordingID),
			action,
		)
		res.OK = true
		res.HTTPStatus = http.StatusOK
		return res

	case MismatchTypeFormatMismatch:
		rl, err := client.UpdateCollectionFormat(
			ctx, action.RecordingID, action.NewFormat,
		)
		if err != nil {
			res.Error, res.HTTPStatus = describeEncoraError(err)
			recordRateLimitBudget(err, rl, sleepBudget)
			return res
		}
		recordEncoraPush(ctx, db,
			fmt.Sprintf("Updated Encora format for %d to %s",
				action.RecordingID, action.NewFormat),
			action,
		)
		res.OK = true
		res.HTTPStatus = http.StatusOK
		return res

	case MismatchTypeMissingFile, MismatchTypeWantedFile:
		// These are flag-only mismatches: the user must download the file
		// before there's anything to push. Once a file is present, the
		// state machine flips to Synced (or AddToCollection for an
		// uncollected wants item) on the next state computation.
		res.Error = "action requires a downloaded file; nothing to push"
		return res

	default:
		res.Error = fmt.Sprintf("unsupported mismatch type %q", action.Type)
		return res
	}
}

// recordRateLimitBudget arms *sleepBudget with the upstream's Retry-
// After (or a sane fallback) when err is ErrRateLimited. Other errors
// leave the budget untouched so an isolated 401 doesn't park the batch.
func recordRateLimitBudget(err error, rl encora.RateLimitInfo, sleepBudget *time.Duration) {
	if !errors.Is(err, encora.ErrRateLimited) {
		return
	}
	if sleepBudget == nil {
		return
	}
	wait := rl.RetryAfter
	if wait <= 0 {
		wait = defaultRetryAfter
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	*sleepBudget = wait
}

// runApplyBatch is the shared driver for both the JSON and HTML apply
// handlers. It validates each submitted action against the current
// mismatch oracle, dispatches the surviving actions through applyOne in
// series, and honors any Retry-After surfaced by an ErrRateLimited
// response by sleeping before issuing the next call (so the next action
// in the batch doesn't immediately re-429 against an upstream that's
// still cooling down). The push loop runs against pushCtx (typically a
// context.WithoutCancel of the request context) so a closed browser
// tab mid-batch doesn't truncate the work or skip the audit log.
func (s *Server) runApplyBatch(
	validateCtx, pushCtx context.Context, actions []ApplyAction,
) ([]ApplyResult, error) {
	v, err := buildValidationSet(validateCtx, s.db)
	if err != nil {
		return nil, err
	}

	results := make([]ApplyResult, 0, len(actions))
	var sleepBudget time.Duration
	for _, action := range actions {
		// Honor any Retry-After captured by the previous iteration
		// before issuing the next call. The sleep is interruptible via
		// pushCtx so a server-shutdown signal still drains promptly.
		if sleepBudget > 0 {
			s.waitForRetry(pushCtx, sleepBudget)
			sleepBudget = 0
		}

		// Reject actions that no longer match the live state — either
		// because the user's collection moved between page render and
		// submit, or because the form was tampered with.
		if rejected, ok := v.validate(action); !ok {
			results = append(results, rejected)
			continue
		}

		results = append(
			results,
			applyOne(pushCtx, s.encora, s.db, action, &sleepBudget),
		)
	}
	return results, nil
}

// waitForRetry sleeps for d via the configured sleeper, but bails out
// early on context cancellation so the batch driver doesn't hold a
// goroutine on a dead request. Tests inject a no-op sleeper to keep
// wall-clock pauses out of the suite.
func (s *Server) waitForRetry(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	s.sleeper(d)
}

// describeEncoraError maps the encora.* sentinel errors onto a (message,
// http-status) pair the JSON + HTML reporters can surface verbatim. Rate-
// limit responses include the upstream's RetryAfter so the user knows
// how long to wait before re-applying.
func describeEncoraError(err error) (string, int) {
	switch {
	case errors.Is(err, encora.ErrUnauthorized):
		return "encora: unauthorized (check API key)", http.StatusUnauthorized
	case errors.Is(err, encora.ErrNotFound):
		return "encora: recording not found", http.StatusNotFound
	case errors.Is(err, encora.ErrRateLimited):
		// Retry-After is honored by the batch driver via
		// recordRateLimitBudget; the user-visible message stays generic
		// because the post-batch summary already reflects the per-action
		// failure shape.
		return "encora: rate limited; retry later", http.StatusTooManyRequests
	default:
		return err.Error(), http.StatusBadGateway
	}
}

// recordEncoraPush writes a HistoryKindEncoraPush row, swallowing the
// error after logging it. A failed history insert must not roll back a
// successful Encora push — the upstream change has already happened.
func recordEncoraPush(
	ctx context.Context, db *ent.Client, summary string, action ApplyAction,
) {
	rid := action.RecordingID
	details := map[string]any{
		"mismatch_type":   string(action.Type),
		recordingIDDetail: action.RecordingID,
	}
	if action.NewFormat != "" {
		details["new_format"] = action.NewFormat
	}
	_, _ = storage.RecordEvent(ctx, db, storage.HistoryEvent{
		Kind:        storage.HistoryKindEncoraPush,
		RecordingID: &rid,
		Summary:     summary,
		Details:     details,
	})
}

// applyJSONRequest is the body shape /api/v1/apply expects.
type applyJSONRequest struct {
	Actions []ApplyAction `json:"actions"`
}

// applyJSONResponse is the result envelope the JSON apply handler returns.
type applyJSONResponse struct {
	Results []ApplyResult `json:"results"`
}

// handleAPIApply is the JSON apply endpoint. Returns 503 when the encora
// client wasn't configured (unauthenticated server runs are still useful
// for read-only views, so we degrade gracefully here rather than refuse
// to start). Per-action errors are reported in the response body, not as
// HTTP errors, so the client can render a full report when only some of
// the batch failed.
//
// The push loop runs on a context.WithoutCancel of the request context
// so a client disconnect mid-batch doesn't leave Encora half-pushed
// without an audit trail. Validation (loadMismatches) keeps using the
// request context because it's local-DB only and a cancelled tab there
// is harmless.
func (s *Server) handleAPIApply(c echo.Context) error {
	if s.encora == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "encora client not configured")
	}

	var req applyJSONRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	reqCtx := c.Request().Context()
	pushCtx := context.WithoutCancel(reqCtx)

	results, err := s.runApplyBatch(reqCtx, pushCtx, req.Actions)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, applyJSONResponse{Results: results})
}

// The HTML apply form-post handler (handleHTMLApply) and its helpers
// (parseApplyForm, splitApplyFormKey, sortInts, decorateApplyResults,
// applyResultPageData / applyResultRow) used to live here. The SPA
// migration retired the server-rendered apply_result.html flow; the
// Mithril client will drive POST /api/v1/apply via JSON in a future
// wave. The legacy templates remain on disk in internal/web/templates/
// as porting reference.
