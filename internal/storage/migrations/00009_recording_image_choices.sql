-- +goose Up
-- +goose StatementBegin
CREATE TABLE recording_image_choices (
    recording_id          INTEGER PRIMARY KEY,
    poster_index          INTEGER,
    backdrop_index        INTEGER,
    overlay_text_override TEXT,
    overlay_style_json    TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (recording_id) REFERENCES recordings(recording_id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS recording_image_choices;
-- +goose StatementEnd
