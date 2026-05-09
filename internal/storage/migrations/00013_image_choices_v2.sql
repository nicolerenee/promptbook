-- +goose Up
-- +goose StatementBegin
-- The single-file-per-slot image cache (v2) drops the indexed-poster /
-- indexed-backdrop selection model. recording_image_choices loses the
-- two index columns; show_image_choices is gone entirely (the chosen
-- show banner is implicit by file existence at shows/<id>/banner.jpg).
ALTER TABLE recording_image_choices DROP COLUMN poster_index;
ALTER TABLE recording_image_choices DROP COLUMN backdrop_index;
DROP TABLE IF EXISTS show_image_choices;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Best-effort restore of the columns + table. The original index
-- values are lost; new rows arrive with NULL.
ALTER TABLE recording_image_choices ADD COLUMN poster_index INTEGER;
ALTER TABLE recording_image_choices ADD COLUMN backdrop_index INTEGER;
CREATE TABLE IF NOT EXISTS show_image_choices (
    show_id      INTEGER PRIMARY KEY,
    poster_index INTEGER,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (show_id) REFERENCES shows(show_id) ON DELETE CASCADE
);
-- +goose StatementEnd
