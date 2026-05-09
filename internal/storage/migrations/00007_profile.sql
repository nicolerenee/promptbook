-- +goose Up
-- +goose StatementBegin
CREATE TABLE profile (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    encora_id          INTEGER NOT NULL,
    name               TEXT NOT NULL DEFAULT '',
    slug               TEXT NOT NULL DEFAULT '',
    username           TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT '',
    recordings_count   INTEGER NOT NULL DEFAULT 0,
    wants_count        INTEGER NOT NULL DEFAULT 0,
    last_seen_at       TEXT NOT NULL DEFAULT '',
    profile_visibility TEXT NOT NULL DEFAULT '',
    col_visibility     TEXT NOT NULL DEFAULT '',
    last_synced_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS profile;
-- +goose StatementEnd
