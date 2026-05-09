package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// EncoraDestructiveClient is the slice of *encora.Client this package
// needs for the user-initiated remove/add endpoints. Surfacing it as a
// separate interface (distinct from EncoraWriteClient) keeps the apply
// flow's surface intentionally minimal — the apply pipeline must never
// be able to push a destructive change "by accident" off a detected
// mismatch. These three methods are only reachable through their own
// dedicated POSTs which the UI gates behind an explicit confirmation
// (Wave 12).
type EncoraDestructiveClient interface {
	RemoveFromCollection(ctx context.Context, id int64) (encora.RateLimitInfo, error)
	RemoveFromWants(ctx context.Context, id int64) (encora.RateLimitInfo, error)
	AddToWants(ctx context.Context, id int64) (encora.RateLimitInfo, error)
}

// recordingIDDetail is the detail-map key the destructive endpoints (and
// apply.go's recordEncoraPush) use to pin a HistoryKindEncoraPush row to
// the affected recording. Lifted into a constant because goconst flags
// the literal once it crosses three call sites.
const recordingIDDetail = "recording_id"

// Compile-time guard: the real *encora.Client must satisfy
// EncoraDestructiveClient so production wiring can pass the same client
// instance for both Encora and EncoraDestructive on Options without a
// wrapper.
var _ EncoraDestructiveClient = (*encora.Client)(nil)

// encoraWriteResponse is the JSON envelope every destructive endpoint
// returns. ok is true on a successful upstream call; error carries the
// human-readable failure reason otherwise.
type encoraWriteResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// parseRecordingIDParam extracts and validates the recording_id path
// parameter. Returns a 400 echo error when the value isn't a positive
// int64.
func parseRecordingIDParam(c echo.Context) (int64, error) {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return id, nil
}

// requireDestructiveClient returns a 503 echo error when the destructive
// client wasn't configured. Mirrors the apply handler's degrade-gracefully
// posture for unauthenticated server runs.
func (s *Server) requireDestructiveClient() (EncoraDestructiveClient, error) {
	if s.encoraDestructive == nil {
		return nil, echo.NewHTTPError(
			http.StatusServiceUnavailable, "encora client not configured")
	}
	return s.encoraDestructive, nil
}

// callDestructive runs a destructive Encora method against a detached
// push context (so a closed browser tab mid-write doesn't strand the
// server in an inconsistent state) and maps the result onto an HTTP
// status + JSON envelope. On success, records the supplied summary as a
// HistoryKindEncoraPush event so the audit log captures every upstream
// change. On failure, surfaces the error and does NOT record history.
func (s *Server) callDestructive(
	c echo.Context,
	id int64,
	action string,
	summary string,
	call func(ctx context.Context) (encora.RateLimitInfo, error),
) error {
	pushCtx := context.WithoutCancel(c.Request().Context())
	_, err := call(pushCtx)
	if err != nil {
		msg, status := describeEncoraError(err)
		return c.JSON(status, encoraWriteResponse{OK: false, Error: msg})
	}

	rid := id
	_, _ = storage.RecordEvent(pushCtx, s.db, storage.HistoryEvent{
		Kind:        storage.HistoryKindEncoraPush,
		RecordingID: &rid,
		Summary:     summary,
		Details: map[string]any{
			"action":          action,
			recordingIDDetail: id,
		},
	})
	return c.JSON(http.StatusOK, encoraWriteResponse{OK: true})
}

// recordingMembership is the trimmed projection of LoadState the
// destructive handlers need to validate that the requested action makes
// sense given current state.
type recordingMembership struct {
	inCollection bool
	inWants      bool
}

// loadRecordingMembership pulls the (in_collection, in_wants) booleans
// for a recording. ErrRecordingMembershipUnknown when the recording isn't
// in either table — the destructive endpoints all require known state, so
// the caller surfaces it as a 404.
func loadRecordingMembership(
	ctx context.Context, db *sql.DB, id int64,
) (recordingMembership, error) {
	var m recordingMembership

	var dummy int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM collection WHERE recording_id = ?`, id).Scan(&dummy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not in collection.
	case err != nil:
		return recordingMembership{}, fmt.Errorf("query collection: %w", err)
	default:
		m.inCollection = true
	}

	err = db.QueryRowContext(ctx,
		`SELECT 1 FROM wants WHERE recording_id = ?`, id).Scan(&dummy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not in wants.
	case err != nil:
		return recordingMembership{}, fmt.Errorf("query wants: %w", err)
	default:
		m.inWants = true
	}

	return m, nil
}

// handleRemoveFromCollection handles POST /api/v1/encora/collection/:id/remove.
// Validates that the recording is currently in the user's collection
// (409 otherwise) before pushing the remove upstream. Successful pushes
// are audit-logged with HistoryKindEncoraPush; failures are not.
func (s *Server) handleRemoveFromCollection(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	client, err := s.requireDestructiveClient()
	if err != nil {
		return err
	}

	m, err := loadRecordingMembership(c.Request().Context(), s.db, id)
	if err != nil {
		return err
	}
	if !m.inCollection {
		return c.JSON(http.StatusConflict, encoraWriteResponse{
			Error: "recording is not in your collection",
		})
	}

	return s.callDestructive(c, id,
		"remove_from_collection",
		fmt.Sprintf("Removed recording %d from Encora collection", id),
		func(ctx context.Context) (encora.RateLimitInfo, error) {
			return client.RemoveFromCollection(ctx, id)
		},
	)
}

// handleRemoveFromWants handles POST /api/v1/encora/wants/:id/remove.
// Validates that the recording is currently on the wants list (409
// otherwise) before pushing the remove upstream.
func (s *Server) handleRemoveFromWants(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	client, err := s.requireDestructiveClient()
	if err != nil {
		return err
	}

	m, err := loadRecordingMembership(c.Request().Context(), s.db, id)
	if err != nil {
		return err
	}
	if !m.inWants {
		return c.JSON(http.StatusConflict, encoraWriteResponse{
			Error: "recording is not on your wants list",
		})
	}

	return s.callDestructive(c, id,
		"remove_from_wants",
		fmt.Sprintf("Removed recording %d from Encora wants", id),
		func(ctx context.Context) (encora.RateLimitInfo, error) {
			return client.RemoveFromWants(ctx, id)
		},
	)
}

// handleAddToWants handles POST /api/v1/encora/wants/:id/add. Rejects
// (409) when the recording is already on the wants list or already in
// the collection — there's no point wanting something you already own.
func (s *Server) handleAddToWants(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	client, err := s.requireDestructiveClient()
	if err != nil {
		return err
	}

	m, err := loadRecordingMembership(c.Request().Context(), s.db, id)
	if err != nil {
		return err
	}
	switch {
	case m.inCollection:
		return c.JSON(http.StatusConflict, encoraWriteResponse{
			Error: "recording is already in your collection",
		})
	case m.inWants:
		return c.JSON(http.StatusConflict, encoraWriteResponse{
			Error: "recording is already on your wants list",
		})
	}

	return s.callDestructive(c, id,
		"add_to_wants",
		fmt.Sprintf("Added recording %d to Encora wants", id),
		func(ctx context.Context) (encora.RateLimitInfo, error) {
			return client.AddToWants(ctx, id)
		},
	)
}
