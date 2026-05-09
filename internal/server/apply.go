package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/encora"
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
func buildValidationSet(ctx context.Context, db *sql.DB) (validationResult, error) {
	items, err := loadMismatches(ctx, db, nil)
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
	db *sql.DB,
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
	ctx context.Context, db *sql.DB, summary string, action ApplyAction,
) {
	rid := action.RecordingID
	details := map[string]any{
		"mismatch_type": string(action.Type),
		"recording_id":  action.RecordingID,
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

// applyResultPageData is the view-model the apply_result.html template
// renders. It pre-counts succeeded/total so the header line stays
// logic-free, and decorates each result with the show metadata loaded by
// loadRecordingMeta so the table reads like a sibling of /mismatches.
// Title / ActiveNav / Version match the shellData shape so the shared
// _layout.html sidebar + topbar render uniformly with the JS-driven
// pages.
type applyResultPageData struct {
	Title     string
	ActiveNav string
	Version   string
	Succeeded int
	Total     int
	Rows      []applyResultRow
}

// applyResultRow combines the apply outcome with display metadata so the
// template doesn't have to do per-row lookups.
type applyResultRow struct {
	Type        MismatchType
	RecordingID int64
	Show        string
	Tour        string
	DateFull    string
	NewFormat   string
	OK          bool
	Error       string
}

// handleHTMLApply is the HTML form post handler. It parses checkboxes
// named action[i].type / action[i].recording_id / action[i].new_format
// out of the form, replays them through applyOne, and renders a result
// table. The push loop is detached from the request context (see
// handleAPIApply) so a closed tab can't strand a half-applied batch
// without history writes.
func (s *Server) handleHTMLApply(c echo.Context) error {
	if s.encora == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "encora client not configured")
	}

	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	actions, err := parseApplyForm(form)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	reqCtx := c.Request().Context()
	pushCtx := context.WithoutCancel(reqCtx)

	results, err := s.runApplyBatch(reqCtx, pushCtx, actions)
	if err != nil {
		return err
	}

	rows, err := decorateApplyResults(pushCtx, s.db, results)
	if err != nil {
		return err
	}

	succeeded := 0
	for _, r := range results {
		if r.OK {
			succeeded++
		}
	}

	return c.Render(http.StatusOK, "apply_result.html", applyResultPageData{
		Title:     "Apply complete",
		ActiveNav: "mismatches",
		Version:   s.version,
		Succeeded: succeeded,
		Total:     len(results),
		Rows:      rows,
	})
}

// parseApplyForm walks the form values and rebuilds the contiguous slice
// of ApplyAction the user selected. Form keys look like
// "action[3].type" / "action[3].recording_id" / "action[3].new_format".
// Indexes that show up in any of the three fields make a candidate
// action; we drop anything missing a recording_id or type since those
// can't form a valid push.
func parseApplyForm(form map[string][]string) ([]ApplyAction, error) {
	type bucket struct {
		typ       string
		recID     string
		newFormat string
		seen      bool
	}
	buckets := map[int]*bucket{}

	for key, vals := range form {
		idx, field, ok := splitApplyFormKey(key)
		if !ok {
			continue
		}
		if len(vals) == 0 {
			continue
		}
		b := buckets[idx]
		if b == nil {
			b = &bucket{}
			buckets[idx] = b
		}
		b.seen = true
		switch field {
		case "type":
			b.typ = vals[0]
		case "recording_id":
			b.recID = vals[0]
		case "new_format":
			b.newFormat = vals[0]
		}
	}

	// Sort indexes so the result order matches the form's ascending
	// index order (Go map iteration is otherwise random).
	idxs := make([]int, 0, len(buckets))
	for i := range buckets {
		idxs = append(idxs, i)
	}
	sortInts(idxs)

	out := make([]ApplyAction, 0, len(buckets))
	for _, idx := range idxs {
		b := buckets[idx]
		if !b.seen || b.typ == "" || b.recID == "" {
			continue
		}
		rid, parseErr := strconv.ParseInt(b.recID, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse recording id %q: %w", b.recID, parseErr)
		}
		out = append(out, ApplyAction{
			Type:        MismatchType(b.typ),
			RecordingID: rid,
			NewFormat:   b.newFormat,
		})
	}
	return out, nil
}

// splitApplyFormKey parses "action[N].FIELD" form-encoded names. Returns
// ok=false for anything that doesn't match the prefix so /apply can
// share its form encoder with future fields without a regex.
func splitApplyFormKey(key string) (int, string, bool) {
	const prefix = "action["
	if len(key) < len(prefix)+3 || key[:len(prefix)] != prefix {
		return 0, "", false
	}
	rest := key[len(prefix):]
	// rest looks like "N].FIELD".
	closer := -1
	for i := range len(rest) {
		if rest[i] == ']' {
			closer = i
			break
		}
	}
	if closer <= 0 || closer+2 > len(rest) || rest[closer+1] != '.' {
		return 0, "", false
	}
	n, err := strconv.Atoi(rest[:closer])
	if err != nil {
		return 0, "", false
	}
	return n, rest[closer+2:], true
}

// sortInts is a tiny helper to keep the parseApplyForm ordering
// deterministic without pulling in sort.Slice for a single-purpose
// integer sort.
func sortInts(s []int) {
	// Insertion sort — the slices are bounded by the number of mismatch
	// rows on screen (handful to a few dozen at most).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// decorateApplyResults loads show/tour/date metadata for the recordings
// referenced by the result list so the rendered table matches the look
// of the mismatches page.
func decorateApplyResults(
	ctx context.Context, db *sql.DB, results []ApplyResult,
) ([]applyResultRow, error) {
	if len(results) == 0 {
		return []applyResultRow{}, nil
	}
	states := make([]storage.RecordingState, 0, len(results))
	for _, r := range results {
		states = append(states, storage.RecordingState{RecordingID: r.Action.RecordingID})
	}
	meta, err := loadRecordingMeta(ctx, db, states)
	if err != nil {
		return nil, err
	}
	rows := make([]applyResultRow, 0, len(results))
	for _, r := range results {
		m := meta[r.Action.RecordingID]
		rows = append(rows, applyResultRow{
			Type:        r.Action.Type,
			RecordingID: r.Action.RecordingID,
			Show:        m.show,
			Tour:        m.tour,
			DateFull:    m.dateFull,
			NewFormat:   r.Action.NewFormat,
			OK:          r.OK,
			Error:       r.Error,
		})
	}
	return rows, nil
}

// handleApplyGet handles GET /apply. The /apply route only does
// meaningful work as a POST (the action list comes from the user's
// selection on /mismatches), so a bare GET redirects rather than
// rendering an empty form.
func (s *Server) handleApplyGet(c echo.Context) error {
	return c.Redirect(http.StatusSeeOther, "/mismatches")
}
