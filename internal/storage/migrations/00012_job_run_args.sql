-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_runs ADD COLUMN args TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_runs DROP COLUMN args;
-- +goose StatementEnd
