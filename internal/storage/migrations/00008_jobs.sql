-- +goose Up
-- +goose StatementBegin
CREATE TABLE job_runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name   TEXT NOT NULL,
    queued_at  TIMESTAMP NOT NULL,
    started_at TIMESTAMP,
    ended_at   TIMESTAMP,
    status     TEXT NOT NULL,
    error      TEXT NOT NULL DEFAULT '',
    trigger    TEXT NOT NULL
);
CREATE INDEX idx_job_runs_name_queued ON job_runs(job_name, queued_at DESC);
CREATE INDEX idx_job_runs_queued ON job_runs(queued_at DESC);

CREATE TABLE job_state (
    job_name         TEXT PRIMARY KEY,
    last_started_at  TIMESTAMP,
    last_ended_at    TIMESTAMP,
    last_duration_ms INTEGER,
    last_status      TEXT
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS job_state;
DROP TABLE IF EXISTS job_runs;
-- +goose StatementEnd
