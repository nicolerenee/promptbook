-- +goose Up
-- +goose StatementBegin

-- Initial placeholder migration. Real schema lands in Phase 2 with `promptbook sync`.
CREATE TABLE schema_marker (
    id INTEGER PRIMARY KEY,
    note TEXT NOT NULL
);

INSERT INTO schema_marker (id, note) VALUES (1, 'promptbook:init');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE schema_marker;

-- +goose StatementEnd
