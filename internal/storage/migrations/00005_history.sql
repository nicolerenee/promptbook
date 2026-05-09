-- +goose Up
-- +goose StatementBegin
CREATE TABLE history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    kind         TEXT NOT NULL,
    recording_id INTEGER,
    summary      TEXT NOT NULL DEFAULT '',
    details_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX history_occurred_at_idx ON history(occurred_at);
CREATE INDEX history_recording_id_idx ON history(recording_id);
CREATE INDEX history_kind_idx ON history(kind);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS history;
-- +goose StatementEnd
