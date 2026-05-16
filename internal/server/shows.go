package server

// shows.go — show-related helpers used by other handlers.
//
// The /api/v1/shows + /api/v1/shows/:id REST endpoints have been
// removed; the SPA reads the show list and detail through GraphQL
// (recordingsList / showsList / show queries on the enrichment
// schema). The image-picker handlers in upload.go + picker_options.go
// still need a quick "does this show id exist" guard, which is what
// showExists provides.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent/show"
)

// showExists returns 404 when no shows row matches id, nil otherwise.
// Used by the upload + picker-options handlers to gate writes against
// a show that hasn't been synced yet.
func (s *Server) showExists(ctx context.Context, id int64) error {
	exists, err := s.db.Show.Query().Where(show.IDEQ(id)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("query show %d: %w", id, err)
	}
	if !exists {
		return echo.NewHTTPError(http.StatusNotFound, "show not found")
	}
	return nil
}
