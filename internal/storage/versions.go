package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
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
func ListVersions(ctx context.Context, db *sql.DB, recordingID int64) ([]RecordingVersion, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, recording_id, file_path, file_size_bytes, container,
		       quality, video_codec, audio_codec, format_label, notes,
		       added_at, last_seen_at
		FROM recording_versions
		WHERE recording_id = ?
		ORDER BY file_size_bytes DESC, id ASC
	`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("query recording_versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecordingVersion
	for rows.Next() {
		var v RecordingVersion
		if scanErr := rows.Scan(
			&v.ID, &v.RecordingID, &v.FilePath, &v.FileSizeBytes,
			&v.Container, &v.Quality, &v.VideoCodec, &v.AudioCodec,
			&v.FormatLabel, &v.Notes, &v.AddedAt, &v.LastSeenAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan recording_version row: %w", scanErr)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recording_versions: %w", err)
	}
	return out, nil
}

// UpsertVersion inserts a recording version or updates it in place when a
// row already exists for (recording_id, file_path). On conflict, every
// mutable field is overwritten with the incoming values except added_at,
// which preserves the original insertion timestamp.
func UpsertVersion(ctx context.Context, db *sql.DB, v RecordingVersion) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO recording_versions (
			recording_id, file_path, file_size_bytes, container,
			quality, video_codec, audio_codec, format_label, notes,
			last_seen_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(recording_id, file_path) DO UPDATE SET
			file_size_bytes = excluded.file_size_bytes,
			container = excluded.container,
			quality = excluded.quality,
			video_codec = excluded.video_codec,
			audio_codec = excluded.audio_codec,
			format_label = excluded.format_label,
			notes = excluded.notes,
			last_seen_at = excluded.last_seen_at
	`,
		v.RecordingID, v.FilePath, v.FileSizeBytes, v.Container,
		v.Quality, v.VideoCodec, v.AudioCodec, v.FormatLabel, v.Notes,
		v.lastSeenOrNow(),
	)
	if err != nil {
		return fmt.Errorf("upsert recording_version: %w", err)
	}
	return nil
}

// DeleteVersion removes a single recording_versions row by primary key.
// Missing rows are not an error — callers reconciling against on-disk
// state may issue deletes optimistically.
func DeleteVersion(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM recording_versions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete recording_version %d: %w", id, err)
	}
	return nil
}

// DeleteVersionsForRecording removes every version row for a recording.
// Useful for full-resync flows that rebuild the per-recording version
// list from scratch.
func DeleteVersionsForRecording(ctx context.Context, db *sql.DB, recordingID int64) error {
	if _, err := db.ExecContext(
		ctx,
		`DELETE FROM recording_versions WHERE recording_id = ?`,
		recordingID,
	); err != nil {
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
