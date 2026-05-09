-- +goose Up
-- +goose StatementBegin
CREATE TABLE manual_import_queue (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path              TEXT NOT NULL UNIQUE,
    file_size_bytes        INTEGER NOT NULL DEFAULT 0,
    discovered_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    suggested_recording_id INTEGER,
    suggested_confidence   TEXT NOT NULL DEFAULT '',
    notes                  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX manual_import_queue_discovered_at_idx ON manual_import_queue(discovered_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS manual_import_queue;
-- +goose StatementEnd
