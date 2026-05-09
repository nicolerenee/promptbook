-- +goose Up
-- +goose StatementBegin
CREATE TABLE performers (
    performer_id INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX performers_name_idx ON performers(name);

CREATE TABLE characters (
    character_id INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS characters;
DROP TABLE IF EXISTS performers;
-- +goose StatementEnd
