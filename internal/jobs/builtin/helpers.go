package builtin

import (
	"context"
	"errors"
	"os"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
)

// DBConn is the *ent.Client alias the refresh-* jobs use. Surfaced as
// a type alias so the job structs can stay tiny without each file re-
// importing the ent package.
type DBConn = ent.Client

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

// performerIDLimit caps how many performer ids we surface for a show.
// Protects /api/images URL length on shows with extreme cast turnover.
const performerIDLimit = 100

// loadPerformerIDsForShow returns up to performerIDLimit distinct
// performer ids credited on any of the show's recordings, ordered
// ascending for stability. Errors are swallowed — the caller falls
// back to the sentinel actor id when this returns nil.
func loadPerformerIDsForShow(ctx context.Context, db *DBConn, showID int64) []int64 {
	if db == nil {
		return nil
	}
	recIDs, err := db.Recording.Query().
		Where(recording.ShowID(showID)).
		IDs(ctx)
	if err != nil || len(recIDs) == 0 {
		return nil
	}
	rows, err := db.CastEntry.Query().
		Where(
			castentry.RecordingIDIn(recIDs...),
			castentry.PerformerIDGT(0),
		).
		Order(castentry.ByPerformerID()).
		All(ctx)
	if err != nil {
		return nil
	}
	seen := make(map[int64]struct{}, len(rows))
	out := make([]int64, 0, len(rows))
	for _, ce := range rows {
		if _, ok := seen[ce.PerformerID]; ok {
			continue
		}
		seen[ce.PerformerID] = struct{}{}
		out = append(out, ce.PerformerID)
		if len(out) >= performerIDLimit {
			break
		}
	}
	return out
}
