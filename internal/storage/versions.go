package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/releaseformat"
)

// RecordingVersion represents a single physical file backing an Encora
// recording. One recording can have multiple versions (e.g. a 2160p
// master alongside a 1080p compressed copy); the recording-level
// format string is rendered via releaseformat.Compose over those
// versions (single-version → bare; multi-version → bracketed, best-
// first by height).
//
// MediaInfoJSON is the JSON-encoded probe.MediaInfo blob captured at
// ingest time. It powers the Sonarr/Radarr-style media-info card on
// the recording detail page; legacy versions imported before the
// field existed leave it as the empty string and the GraphQL resolver
// returns a null mediaInfo in that case.
//
// SourceFolder is the directory the source file lived in at ingest
// time, captured for folder-as-unit drops so the recording detail
// page can enumerate sibling files (audio/, photos/, etc.) as
// 'extras'. Empty for loose-file imports — the recording's
// destination folder is the only location with content in that case.
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
	MediaInfoJSON string
	SourceFolder  string
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

// VersionExistsByPath returns true when any recording_versions row
// references the exact file_path. Useful for scanner-style flows that
// want a single yes/no decision without hydrating the full row.
func VersionExistsByPath(ctx context.Context, client *ent.Client, path string) (bool, error) {
	exists, err := client.RecordingVersion.Query().
		Where(recordingversion.FilePath(path)).
		Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("query recording_versions by path: %w", err)
	}
	return exists, nil
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
		SetMediaInfoJSON(v.MediaInfoJSON).
		SetSourceFolder(v.SourceFolder).
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
			u.UpdateMediaInfoJSON()
			u.UpdateSourceFolder()
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
// state may issue deletes optimistically.
func DeleteVersion(ctx context.Context, client *ent.Client, id int64) error {
	_, err := client.RecordingVersion.Delete().
		Where(recordingversion.IDEQ(id)).
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

// ComputeFormatString returns the canonical release-format string for
// a recording's versions — the same shape the SPA renders + the same
// shape we push to encora's release_format field on a mismatch
// resolution. Single version → bare "MP4 - x265 / AAC - 2160p - 8.57 GB";
// multi-version → bracketed "[best] [next] …" sorted best-first.
//
// Each version's MediaInfo comes from the persisted media_info_json
// blob (populated at ingest time via ffprobe). Legacy versions
// without a blob render with "?" placeholders for the missing fields
// — informative even on imports that pre-date the probe pipeline.
//
// Caller is responsible for passing versions in roughly desired
// order; Compose stable-sorts on Height descending, so the input
// order only matters for ties at the same height.
func ComputeFormatString(versions []RecordingVersion) string {
	infos := make([]releaseformat.VersionInfo, 0, len(versions))
	for _, v := range versions {
		var mi probe.MediaInfo
		if v.MediaInfoJSON != "" {
			// A malformed blob shouldn't crash the format helper —
			// we fall through to the empty MediaInfo case which still
			// produces a useful "{ext} - ? / ? - ? - {size}" line.
			_ = json.Unmarshal([]byte(v.MediaInfoJSON), &mi)
		}
		ext := filepath.Ext(v.FilePath)
		infos = append(infos,
			releaseformat.FromVersionAndMediaInfo(mi, v.FileSizeBytes, ext))
	}
	return releaseformat.Compose(infos)
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
		ID:            r.ID,
		RecordingID:   r.RecordingID,
		FilePath:      r.FilePath,
		FileSizeBytes: r.FileSizeBytes,
		Container:     r.Container,
		Quality:       r.Quality,
		VideoCodec:    r.VideoCodec,
		AudioCodec:    r.AudioCodec,
		FormatLabel:   r.FormatLabel,
		Notes:         r.Notes,
		MediaInfoJSON: r.MediaInfoJSON,
		SourceFolder:  r.SourceFolder,
		AddedAt:       r.AddedAt,
		LastSeenAt:    r.LastSeenAt,
	}
}
