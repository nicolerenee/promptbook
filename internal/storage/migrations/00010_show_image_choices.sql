-- +goose Up
-- +goose StatementBegin
CREATE TABLE show_image_choices (
    show_id      INTEGER PRIMARY KEY,
    poster_index INTEGER,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (show_id) REFERENCES shows(show_id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS show_image_choices;
-- +goose StatementEnd
