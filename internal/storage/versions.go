package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
)

// FormatSeparator is the delimiter joining per-version format labels into
// the single Encora-style format string surfaced on a recording.
const FormatSeparator = " | "

// RecordingVersion represents a single physical file backing an Encora
// recording. One recording can have multiple versions (e.g. a 2160p
// master alongside a 1080p compressed copy); the per-version format
// labels are joined with FormatSeparator to form the recording-level
// format string.
type RecordingVersion struct {
	ID            int64
	RecordingID   int64
	FilePath      string
	FileSizeBytes int64
	Container     string
	Quality       string
	VideoCodec    string
	AudioCodec    string
	FormatLabel   string
	Notes         string
	AddedAt       time.Time
	LastSeenAt    time.Time
}

// ListVersions returns every recording_versions row for the given
// recording, largest file first then by insertion order. The order is
// deliberate: ComputeFormatString relies on it so the canonical format
// string leads with the highest-quality master.
func ListVersions(
	ctx context.Context,
	client *ent.Client,
	recordingID int64,
) ([]RecordingVersion, error) {
	rows, err := client.RecordingVersion.Query().
		Where(recordingversion.RecordingID(recordingID)).
		Order(
			recordingversion.ByFileSizeBytes(entsql.OrderDesc()),
			recordingversion.ByID(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query recording_versions: %w", err)
	}
	out := make([]RecordingVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordingVersionFromEnt(r))
	}
	return out, nil
}

// UpsertVersion inserts a recording version or updates it in place when a
// row already exists for (recording_id, file_path). On conflict, every
// mutable field is overwritten with the incoming values except added_at,
// which preserves the original insertion timestamp.
func UpsertVersion(ctx context.Context, client *ent.Client, v RecordingVersion) error {
	return upsertVersion(ctx, client.RecordingVersion.Create(), v)
}

// UpsertVersionTx is the transaction-scoped sibling of UpsertVersion for
// callers that already hold an *ent.Tx.
func UpsertVersionTx(ctx context.Context, tx *ent.Tx, v RecordingVersion) error {
	return upsertVersion(ctx, tx.RecordingVersion.Create(), v)
}

func upsertVersion(
	ctx context.Context,
	c *ent.RecordingVersionCreate,
	v RecordingVersion,
) error {
	// On conflict, refresh every mutable field except added_at — the
	// original insertion timestamp is part of the row's identity for
	// audit purposes. UpdateNewValues would clobber it, so we name
	// each updatable column explicitly.
	err := c.
		SetRecordingID(v.RecordingID).
		SetFilePath(v.FilePath).
		SetFileSizeBytes(v.FileSizeBytes).
		SetContainer(v.Container).
		SetQuality(v.Quality).
		SetVideoCodec(v.VideoCodec).
		SetAudioCodec(v.AudioCodec).
		SetFormatLabel(v.FormatLabel).
		SetNotes(v.Notes).
		SetLastSeenAt(v.lastSeenOrNow()).
		OnConflictColumns(
			recordingversion.FieldRecordingID,
			recordingversion.FieldFilePath,
		).
		Update(func(u *ent.RecordingVersionUpsert) {
			u.UpdateFileSizeBytes()
			u.UpdateContainer()
			u.UpdateQuality()
			u.UpdateVideoCodec()
			u.UpdateAudioCodec()
			u.UpdateFormatLabel()
			u.UpdateNotes()
			u.UpdateLastSeenAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert recording_version: %w", err)
	}
	return nil
}

// DeleteVersion removes a single recording_versions row by primary key.
// Missing rows are not an error — callers reconciling against on-disk
// state may issue deletes optimistically. id is taken as int64 for
// caller convenience; the ent ID column is a smaller int but the cache
// would require a billion rows before truncation matters.
func DeleteVersion(ctx context.Context, client *ent.Client, id int64) error {
	_, err := client.RecordingVersion.Delete().
		Where(recordingversion.IDEQ(int(id))).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete recording_version %d: %w", id, err)
	}
	return nil
}

// DeleteVersionsForRecording removes every version row for a recording.
// Useful for full-resync flows that rebuild the per-recording version
// list from scratch.
func DeleteVersionsForRecording(
	ctx context.Context,
	client *ent.Client,
	recordingID int64,
) error {
	_, err := client.RecordingVersion.Delete().
		Where(recordingversion.RecordingID(recordingID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete recording_versions for %d: %w", recordingID, err)
	}
	return nil
}

// ComputeFormatString joins each version's FormatLabel with
// FormatSeparator. Empty labels are skipped; if every version has an
// empty label the result is the empty string. Caller is responsible for
// passing versions in the desired order (typically the order returned by
// ListVersions, i.e. largest file first).
func ComputeFormatString(versions []RecordingVersion) string {
	labels := make([]string, 0, len(versions))
	for _, v := range versions {
		if v.FormatLabel == "" {
			continue
		}
		labels = append(labels, v.FormatLabel)
	}
	return strings.Join(labels, FormatSeparator)
}

// lastSeenOrNow returns the version's LastSeenAt timestamp, or the
// current time if the caller left it as the zero value. SQLite's
// CURRENT_TIMESTAMP default does not apply when an explicit value is
// passed via parameter, so we substitute here to keep last_seen_at
// monotonic without forcing every caller to set a clock.
func (v RecordingVersion) lastSeenOrNow() time.Time {
	if v.LastSeenAt.IsZero() {
		return time.Now().UTC()
	}
	return v.LastSeenAt
}

// recordingVersionFromEnt converts an ent.RecordingVersion to the
// package-local RecordingVersion DTO. Callers consume the DTO so the
// JSON-wire shape stays decoupled from the ORM.
func recordingVersionFromEnt(r *ent.RecordingVersion) RecordingVersion {
	return RecordingVersion{
		ID:            int64(r.ID),
		RecordingID:   r.RecordingID,
		FilePath:      r.FilePath,
		FileSizeBytes: r.FileSizeBytes,
		Container:     r.Container,
		Quality:       r.Quality,
		VideoCodec:    r.VideoCodec,
		AudioCodec:    r.AudioCodec,
		FormatLabel:   r.FormatLabel,
		Notes:         r.Notes,
		AddedAt:       r.AddedAt,
		LastSeenAt:    r.LastSeenAt,
	}
}
