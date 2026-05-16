package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/extraentry"
)

// RecordingExtra is the package-local DTO for a `recording_extras`
// row. Callers stay decoupled from the ent schema's `ExtraEntry` Go
// type — that name dodges a gqlgen autobind collision with the
// hand-rolled GraphQL `RecordingExtra` model. Storage callers want
// the more obvious name; the rename is local to the ent layer.
type RecordingExtra struct {
	ID            int64
	RecordingID   int64
	FilePath      string
	Kind          string
	Label         string
	FileSizeBytes int64
	AddedAt       time.Time
}

// UpsertExtra inserts a recording_extras row or updates the existing
// (recording_id, file_path) match's kind / label / size in place. The
// added_at timestamp survives the upsert so the original insertion
// time is preserved across re-ingests of the same file. Returns the
// row id.
func UpsertExtra(ctx context.Context, client *ent.Client, e RecordingExtra) (int64, error) {
	id, err := client.ExtraEntry.Create().
		SetRecordingID(e.RecordingID).
		SetFilePath(e.FilePath).
		SetKind(e.Kind).
		SetLabel(e.Label).
		SetFileSizeBytes(e.FileSizeBytes).
		OnConflictColumns(
			extraentry.FieldRecordingID,
			extraentry.FieldFilePath,
		).
		Update(func(u *ent.ExtraEntryUpsert) {
			u.UpdateKind()
			u.UpdateLabel()
			u.UpdateFileSizeBytes()
		}).
		ID(ctx)
	if err != nil {
		return 0, fmt.Errorf("upsert recording_extra: %w", err)
	}
	return id, nil
}

// ListExtras returns every recording_extras row for the given
// recording, ordered by file_path (lexicographic) so the SPA's tree
// renderer sees a stable, deterministic order regardless of insertion
// sequence.
func ListExtras(ctx context.Context, client *ent.Client, recordingID int64) ([]RecordingExtra, error) {
	rows, err := client.ExtraEntry.Query().
		Where(extraentry.RecordingID(recordingID)).
		Order(extraentry.ByFilePath()).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query recording_extras: %w", err)
	}
	out := make([]RecordingExtra, 0, len(rows))
	for _, r := range rows {
		out = append(out, RecordingExtra{
			ID:            r.ID,
			RecordingID:   r.RecordingID,
			FilePath:      r.FilePath,
			Kind:          r.Kind,
			Label:         r.Label,
			FileSizeBytes: r.FileSizeBytes,
			AddedAt:       r.AddedAt,
		})
	}
	return out, nil
}
