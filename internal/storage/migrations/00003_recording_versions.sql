-- +goose Up
-- +goose StatementBegin
CREATE TABLE recording_versions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    recording_id    INTEGER NOT NULL REFERENCES recordings(recording_id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    file_size_bytes INTEGER NOT NULL DEFAULT 0,
    container       TEXT NOT NULL DEFAULT '',
    quality         TEXT NOT NULL DEFAULT '',
    video_codec     TEXT NOT NULL DEFAULT '',
    audio_codec     TEXT NOT NULL DEFAULT '',
    format_label    TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    added_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX recording_versions_recording_id_idx ON recording_versions(recording_id);
CREATE UNIQUE INDEX recording_versions_recording_path_uidx ON recording_versions(recording_id, file_path);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS recording_versions;
-- +goose StatementEnd
