-- +goose Up
-- +goose StatementBegin

-- Drop the placeholder table from migration 00001 — promptbook only ever
-- shipped to one user (the author) so there's no rollout cost.
DROP TABLE IF EXISTS schema_marker;

-- Shows: one row per Encora show. recordings.show_id FKs in here.
CREATE TABLE shows (
    show_id          INTEGER PRIMARY KEY,
    name             TEXT NOT NULL,
    description_html TEXT NOT NULL DEFAULT '',
    last_seen_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Recordings: one row per Encora recording. raw_json holds the original
-- payload so we can re-derive any field we forgot to denormalize without
-- re-fetching from the API.
CREATE TABLE recordings (
    recording_id          INTEGER PRIMARY KEY,
    show_id               INTEGER NOT NULL REFERENCES shows(show_id),
    tour                  TEXT NOT NULL,
    date_full             TEXT NOT NULL,
    date_month_known      INTEGER NOT NULL DEFAULT 0,
    date_day_known        INTEGER NOT NULL DEFAULT 0,
    date_variant          TEXT,
    date_time             TEXT NOT NULL DEFAULT 'unknown',
    master                TEXT NOT NULL DEFAULT '',
    nft_date              TEXT,
    nft_forever           INTEGER NOT NULL DEFAULT 0,
    notes                 TEXT NOT NULL DEFAULT '',
    master_notes          TEXT,
    release_format        TEXT,
    venue                 TEXT NOT NULL DEFAULT '',
    city                  TEXT NOT NULL DEFAULT '',
    media_type            TEXT NOT NULL DEFAULT '',
    recording_type        TEXT NOT NULL DEFAULT '',
    amount_recorded       TEXT NOT NULL DEFAULT '',
    gifting_status        TEXT NOT NULL DEFAULT '',
    limited_status        TEXT NOT NULL DEFAULT '',
    is_opening            INTEGER NOT NULL DEFAULT 0,
    is_closing            INTEGER NOT NULL DEFAULT 0,
    is_preview            INTEGER NOT NULL DEFAULT 0,
    is_concert            INTEGER NOT NULL DEFAULT 0,
    is_nfs                INTEGER NOT NULL DEFAULT 0,
    is_favourite          INTEGER NOT NULL DEFAULT 0,
    has_screenshots       INTEGER NOT NULL DEFAULT 0,
    has_subtitles         INTEGER NOT NULL DEFAULT 0,
    boot_camp_recommended INTEGER NOT NULL DEFAULT 0,
    owners_count          INTEGER NOT NULL DEFAULT 0,
    wanters_count         INTEGER NOT NULL DEFAULT 0,
    last_updated          TEXT NOT NULL DEFAULT '',
    raw_json              TEXT NOT NULL,
    last_seen_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX recordings_show_id_idx ON recordings(show_id);

-- Cast entries: many per recording. Cascade so dropping a recording
-- (e.g. orphan cleanup) clears its cast.
CREATE TABLE cast_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    recording_id    INTEGER NOT NULL REFERENCES recordings(recording_id) ON DELETE CASCADE,
    performer_id    INTEGER NOT NULL,
    performer_name  TEXT NOT NULL,
    performer_slug  TEXT NOT NULL DEFAULT '',
    performer_url   TEXT NOT NULL DEFAULT '',
    character_id    INTEGER NOT NULL,
    character_name  TEXT NOT NULL,
    character_slug  TEXT NOT NULL DEFAULT '',
    character_url   TEXT NOT NULL DEFAULT '',
    character_order INTEGER NOT NULL DEFAULT 0,
    status_label    TEXT,
    status_abbrev   TEXT
);

CREATE INDEX cast_entries_recording_id_idx ON cast_entries(recording_id);
CREATE INDEX cast_entries_performer_id_idx ON cast_entries(performer_id);

-- Collection: 1:1 with the user's owned recordings. recording_id is the
-- shared join key with recordings.
CREATE TABLE collection (
    recording_id    INTEGER PRIMARY KEY REFERENCES recordings(recording_id) ON DELETE CASCADE,
    format          TEXT NOT NULL DEFAULT '',
    user_notes      TEXT,
    user_watched    INTEGER NOT NULL DEFAULT 0,
    collected_at    DATETIME,
    updated_at      DATETIME,
    last_synced_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Wants: 1:1 with the user's wants list. The wire format only carries the
-- recording payload — no priority or added_at fields.
CREATE TABLE wants (
    recording_id   INTEGER PRIMARY KEY REFERENCES recordings(recording_id) ON DELETE CASCADE,
    last_synced_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Sync run log. One row per call to sync.Sync. error_text is empty
-- on success, holds the first fatal error otherwise.
CREATE TABLE sync_runs (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                 TEXT NOT NULL,
    started_at           DATETIME NOT NULL,
    finished_at          DATETIME,
    ok_count             INTEGER NOT NULL DEFAULT 0,
    error_count          INTEGER NOT NULL DEFAULT 0,
    rate_limit_remaining INTEGER NOT NULL DEFAULT 0,
    error_text           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX sync_runs_started_at_idx ON sync_runs(started_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS sync_runs;
DROP TABLE IF EXISTS wants;
DROP TABLE IF EXISTS collection;
DROP TABLE IF EXISTS cast_entries;
DROP TABLE IF EXISTS recordings;
DROP TABLE IF EXISTS shows;

CREATE TABLE schema_marker (
    id   INTEGER PRIMARY KEY,
    note TEXT NOT NULL
);

INSERT INTO schema_marker (id, note) VALUES (1, 'promptbook:init');

-- +goose StatementEnd
