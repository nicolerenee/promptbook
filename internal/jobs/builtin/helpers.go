package builtin

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/nicolerenee/promptbook/internal/stagemedia"
)

// DBConn is the *sql.DB alias the refresh-* jobs use. Surfaced as a
// type alias so the job structs can stay tiny without each file
// re-importing database/sql.
type DBConn = sql.DB

// stagemediaPerformer aliases stagemedia.Performer so refresh-show-images
// can iterate the response without importing the stagemedia package
// at the jobs/builtin layer's surface (the type is concrete in the
// pbsync.StagemediaImageClient.Images return value).
type stagemediaPerformer = stagemedia.Performer

// firstNonEmpty returns the first non-empty string from urls, or ""
// when every entry is empty / urls is nil.
func firstNonEmpty(urls []string) string {
	for _, u := range urls {
		if u != "" {
			return u
		}
	}
	return ""
}

// removeIfExists removes path, ignoring os.ErrNotExist. Other errors
// propagate. Used by --force fan-out paths to clear an existing slot
// before re-fetching.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// performerIDInitialCap is the default capacity for the performer-id
// slice. Most shows have well under this many distinct performers
// across their recordings; an under-allocation just costs one slice
// growth, not correctness.
const performerIDInitialCap = 16

// loadPerformerIDsForShow returns up to a hundred distinct performer
// ids credited on any of the show's recordings, ordered for stability.
// The cap protects /api/images URL length on shows with extreme cast
// turnover. Errors are swallowed — the caller falls back to the
// sentinel actor id when this returns nil.
func loadPerformerIDsForShow(ctx context.Context, db *DBConn, showID int64) []int64 {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT ce.performer_id
		FROM cast_entries ce
		JOIN recordings r ON r.recording_id = ce.recording_id
		WHERE r.show_id = ?
		  AND ce.performer_id IS NOT NULL
		  AND ce.performer_id > 0
		ORDER BY ce.performer_id
		LIMIT 100
	`, showID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	out := make([]int64, 0, performerIDInitialCap)
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			return out
		}
		out = append(out, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return out
	}
	return out
}
