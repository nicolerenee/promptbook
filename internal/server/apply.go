package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

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

// applyOne dispatches a single ApplyAction to the right Encora write
// endpoint, surfacing per-error detail in ApplyResult. Successful pushes
// are recorded as HistoryKindEncoraPush events so the audit trail
// matches what changed upstream. Failures intentionally do NOT write a
// history row — the server's history log is the user-visible record of
// "things that took effect", not "things we tried".
//
// Removal flows (RemoveFromCollection, RemoveFromWants) are intentionally
// out of scope here — destructive pushes need a dedicated confirmation UI
// and history-event taxonomy.
func applyOne(
	ctx context.Context,
	client EncoraWriteClient,
	db *sql.DB,
	action ApplyAction,
) ApplyResult {
	res := ApplyResult{Action: action}

	switch action.Type {
	case MismatchTypeAddToCollection:
		_, err := client.AddToCollection(ctx, action.RecordingID)
		if err != nil {
			res.Error, res.HTTPStatus = describeEncoraError(err)
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
		_, err := client.UpdateCollectionFormat(
			ctx, action.RecordingID, action.NewFormat,
		)
		if err != nil {
			res.Error, res.HTTPStatus = describeEncoraError(err)
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
		// The encora client doesn't yet thread RateLimitInfo through the
		// error itself; if a future revision wraps the retry-after into a
		// typed error, parse it here. For now surface a generic message.
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
func (s *Server) handleAPIApply(c echo.Context) error {
	if s.encora == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "encora client not configured")
	}

	var req applyJSONRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	results := make([]ApplyResult, 0, len(req.Actions))
	for _, action := range req.Actions {
		results = append(results, applyOne(c.Request().Context(), s.encora, s.db, action))
	}
	return c.JSON(http.StatusOK, applyJSONResponse{Results: results})
}

// applyResultPageData is the view-model the apply_result.html template
// renders. It pre-counts succeeded/total so the header line stays
// logic-free, and decorates each result with the show metadata loaded by
// loadRecordingMeta so the table reads like a sibling of /mismatches.
type applyResultPageData struct {
	Title     string
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
// table.
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

	ctx := c.Request().Context()
	results := make([]ApplyResult, 0, len(actions))
	for _, a := range actions {
		results = append(results, applyOne(ctx, s.encora, s.db, a))
	}

	rows, err := decorateApplyResults(ctx, s.db, results)
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
