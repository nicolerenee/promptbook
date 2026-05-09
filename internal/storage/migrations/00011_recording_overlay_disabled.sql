-- +goose Up
-- +goose StatementBegin
ALTER TABLE recording_image_choices ADD COLUMN overlay_disabled INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- SQLite ALTER TABLE DROP COLUMN landed in 3.35; goose's bundled engine
-- supports it. Falls back to a no-op rename pattern on older engines.
ALTER TABLE recording_image_choices DROP COLUMN overlay_disabled;
-- +goose StatementEnd
