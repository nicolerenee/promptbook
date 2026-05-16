-- +goose Up
-- create "cast_entries" table
CREATE TABLE `cast_entries` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `performer_id` integer NOT NULL, `performer_name` text NOT NULL, `performer_slug` text NOT NULL DEFAULT (''), `performer_url` text NOT NULL DEFAULT (''), `character_id` integer NOT NULL, `character_name` text NOT NULL, `character_slug` text NOT NULL DEFAULT (''), `character_url` text NOT NULL DEFAULT (''), `character_order` integer NOT NULL DEFAULT (0), `status_label` text NULL, `status_abbrev` text NULL, `recording_id` integer NOT NULL, CONSTRAINT `cast_entries_recordings_cast_entries` FOREIGN KEY (`recording_id`) REFERENCES `recordings` (`recording_id`) ON DELETE CASCADE);
-- create index "castentry_recording_id" to table: "cast_entries"
CREATE INDEX `castentry_recording_id` ON `cast_entries` (`recording_id`);
-- create index "castentry_performer_id" to table: "cast_entries"
CREATE INDEX `castentry_performer_id` ON `cast_entries` (`performer_id`);
-- create "characters" table
CREATE TABLE `characters` (`character_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `name` text NOT NULL, `slug` text NOT NULL DEFAULT (''), `url` text NOT NULL DEFAULT (''), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP));
-- create "collection" table
CREATE TABLE `collection` (`recording_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `format` text NOT NULL DEFAULT (''), `user_notes` text NULL, `user_watched` bool NOT NULL DEFAULT (false), `collected_at` datetime NULL, `updated_at` datetime NULL, `last_synced_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP));
-- create "history" table
CREATE TABLE `history` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `occurred_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `kind` text NOT NULL, `recording_id` integer NULL, `summary` text NOT NULL DEFAULT (''), `details_json` text NOT NULL DEFAULT ('{}'));
-- create index "historyevent_occurred_at" to table: "history"
CREATE INDEX `historyevent_occurred_at` ON `history` (`occurred_at`);
-- create index "historyevent_recording_id" to table: "history"
CREATE INDEX `historyevent_recording_id` ON `history` (`recording_id`);
-- create index "historyevent_kind" to table: "history"
CREATE INDEX `historyevent_kind` ON `history` (`kind`);
-- create "job_runs" table
CREATE TABLE `job_runs` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `job_name` text NOT NULL, `queued_at` timestamp NOT NULL, `started_at` timestamp NULL, `ended_at` timestamp NULL, `status` text NOT NULL, `error` text NOT NULL DEFAULT (''), `trigger` text NOT NULL, `args` text NOT NULL DEFAULT (''));
-- create index "idx_job_runs_name_queued" to table: "job_runs"
CREATE INDEX `idx_job_runs_name_queued` ON `job_runs` (`job_name`, `queued_at`);
-- create index "idx_job_runs_queued" to table: "job_runs"
CREATE INDEX `idx_job_runs_queued` ON `job_runs` (`queued_at` DESC);
-- create "job_state" table
CREATE TABLE `job_state` (`job_name` text NOT NULL, `last_started_at` timestamp NULL, `last_ended_at` timestamp NULL, `last_duration_ms` integer NULL, `last_status` text NULL, PRIMARY KEY (`job_name`));
-- create "manual_import_queue" table
CREATE TABLE `manual_import_queue` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `file_path` text NOT NULL, `file_size_bytes` integer NOT NULL DEFAULT (0), `discovered_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `suggested_recording_id` integer NULL, `suggested_confidence` text NOT NULL DEFAULT (''), `notes` text NOT NULL DEFAULT (''));
-- create index "manual_import_queue_file_path_key" to table: "manual_import_queue"
CREATE UNIQUE INDEX `manual_import_queue_file_path_key` ON `manual_import_queue` (`file_path`);
-- create index "manualimportqueue_discovered_at" to table: "manual_import_queue"
CREATE INDEX `manualimportqueue_discovered_at` ON `manual_import_queue` (`discovered_at`);
-- create "performers" table
CREATE TABLE `performers` (`performer_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `name` text NOT NULL, `slug` text NOT NULL DEFAULT (''), `url` text NOT NULL DEFAULT (''), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP));
-- create index "performer_name" to table: "performers"
CREATE INDEX `performer_name` ON `performers` (`name`);
-- create "profile" table
CREATE TABLE `profile` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `encora_id` integer NOT NULL, `name` text NOT NULL DEFAULT (''), `slug` text NOT NULL DEFAULT (''), `username` text NOT NULL DEFAULT (''), `status` text NOT NULL DEFAULT (''), `recordings_count` integer NOT NULL DEFAULT (0), `wants_count` integer NOT NULL DEFAULT (0), `last_seen_at` text NOT NULL DEFAULT (''), `profile_visibility` text NOT NULL DEFAULT (''), `col_visibility` text NOT NULL DEFAULT (''), `last_synced_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), CHECK (id = 1));
-- create "recordings" table
CREATE TABLE `recordings` (`recording_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `tour` text NOT NULL DEFAULT (''), `date_full` text NOT NULL DEFAULT (''), `date_month_known` bool NOT NULL DEFAULT (false), `date_day_known` bool NOT NULL DEFAULT (false), `date_variant` text NULL, `date_time` text NOT NULL DEFAULT ('unknown'), `master` text NOT NULL DEFAULT (''), `nft_date` text NULL, `nft_forever` bool NOT NULL DEFAULT (false), `notes` text NOT NULL DEFAULT (''), `master_notes` text NULL, `release_format` text NULL, `venue` text NOT NULL DEFAULT (''), `city` text NOT NULL DEFAULT (''), `media_type` text NOT NULL DEFAULT (''), `recording_type` text NOT NULL DEFAULT (''), `amount_recorded` text NOT NULL DEFAULT (''), `gifting_status` text NOT NULL DEFAULT (''), `limited_status` text NOT NULL DEFAULT (''), `is_opening` bool NOT NULL DEFAULT (false), `is_closing` bool NOT NULL DEFAULT (false), `is_preview` bool NOT NULL DEFAULT (false), `is_concert` bool NOT NULL DEFAULT (false), `is_nfs` bool NOT NULL DEFAULT (false), `is_favourite` bool NOT NULL DEFAULT (false), `has_screenshots` bool NOT NULL DEFAULT (false), `has_subtitles` bool NOT NULL DEFAULT (false), `boot_camp_recommended` bool NOT NULL DEFAULT (false), `owners_count` integer NOT NULL DEFAULT (0), `wanters_count` integer NOT NULL DEFAULT (0), `last_updated` text NOT NULL DEFAULT (''), `raw_json` text NOT NULL, `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `show_id` integer NOT NULL, CONSTRAINT `recordings_shows_recordings` FOREIGN KEY (`show_id`) REFERENCES `shows` (`show_id`) ON DELETE NO ACTION);
-- create index "recording_show_id" to table: "recordings"
CREATE INDEX `recording_show_id` ON `recordings` (`show_id`);
-- create "recording_image_choices" table
CREATE TABLE `recording_image_choices` (`recording_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `overlay_text_override` text NULL, `overlay_style_json` text NULL, `overlay_disabled` bool NOT NULL DEFAULT (false), `updated_at` timestamp NOT NULL DEFAULT (CURRENT_TIMESTAMP));
-- create "recording_versions" table
CREATE TABLE `recording_versions` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `file_path` text NOT NULL, `file_size_bytes` integer NOT NULL DEFAULT (0), `container` text NOT NULL DEFAULT (''), `quality` text NOT NULL DEFAULT (''), `video_codec` text NOT NULL DEFAULT (''), `audio_codec` text NOT NULL DEFAULT (''), `format_label` text NOT NULL DEFAULT (''), `notes` text NOT NULL DEFAULT (''), `added_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `recording_id` integer NOT NULL, CONSTRAINT `recording_versions_recordings_versions` FOREIGN KEY (`recording_id`) REFERENCES `recordings` (`recording_id`) ON DELETE CASCADE);
-- create index "recordingversion_recording_id" to table: "recording_versions"
CREATE INDEX `recordingversion_recording_id` ON `recording_versions` (`recording_id`);
-- create index "recordingversion_recording_id_file_path" to table: "recording_versions"
CREATE UNIQUE INDEX `recordingversion_recording_id_file_path` ON `recording_versions` (`recording_id`, `file_path`);
-- create "shows" table
CREATE TABLE `shows` (`show_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `name` text NOT NULL, `description_html` text NOT NULL DEFAULT (''), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP));
-- create "sync_runs" table
CREATE TABLE `sync_runs` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `kind` text NOT NULL, `started_at` datetime NOT NULL, `finished_at` datetime NULL, `ok_count` integer NOT NULL DEFAULT (0), `error_count` integer NOT NULL DEFAULT (0), `rate_limit_remaining` integer NOT NULL DEFAULT (0), `error_text` text NOT NULL DEFAULT (''));
-- create index "syncrun_started_at" to table: "sync_runs"
CREATE INDEX `syncrun_started_at` ON `sync_runs` (`started_at`);
-- create "wants" table
CREATE TABLE `wants` (`recording_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `last_synced_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP));

-- +goose Down
-- reverse: create "wants" table
DROP TABLE `wants`;
-- reverse: create index "syncrun_started_at" to table: "sync_runs"
DROP INDEX `syncrun_started_at`;
-- reverse: create "sync_runs" table
DROP TABLE `sync_runs`;
-- reverse: create "shows" table
DROP TABLE `shows`;
-- reverse: create index "recordingversion_recording_id_file_path" to table: "recording_versions"
DROP INDEX `recordingversion_recording_id_file_path`;
-- reverse: create index "recordingversion_recording_id" to table: "recording_versions"
DROP INDEX `recordingversion_recording_id`;
-- reverse: create "recording_versions" table
DROP TABLE `recording_versions`;
-- reverse: create "recording_image_choices" table
DROP TABLE `recording_image_choices`;
-- reverse: create index "recording_show_id" to table: "recordings"
DROP INDEX `recording_show_id`;
-- reverse: create "recordings" table
DROP TABLE `recordings`;
-- reverse: create "profile" table
DROP TABLE `profile`;
-- reverse: create index "performer_name" to table: "performers"
DROP INDEX `performer_name`;
-- reverse: create "performers" table
DROP TABLE `performers`;
-- reverse: create index "manualimportqueue_discovered_at" to table: "manual_import_queue"
DROP INDEX `manualimportqueue_discovered_at`;
-- reverse: create index "manual_import_queue_file_path_key" to table: "manual_import_queue"
DROP INDEX `manual_import_queue_file_path_key`;
-- reverse: create "manual_import_queue" table
DROP TABLE `manual_import_queue`;
-- reverse: create "job_state" table
DROP TABLE `job_state`;
-- reverse: create index "idx_job_runs_queued" to table: "job_runs"
DROP INDEX `idx_job_runs_queued`;
-- reverse: create index "idx_job_runs_name_queued" to table: "job_runs"
DROP INDEX `idx_job_runs_name_queued`;
-- reverse: create "job_runs" table
DROP TABLE `job_runs`;
-- reverse: create index "historyevent_kind" to table: "history"
DROP INDEX `historyevent_kind`;
-- reverse: create index "historyevent_recording_id" to table: "history"
DROP INDEX `historyevent_recording_id`;
-- reverse: create index "historyevent_occurred_at" to table: "history"
DROP INDEX `historyevent_occurred_at`;
-- reverse: create "history" table
DROP TABLE `history`;
-- reverse: create "collection" table
DROP TABLE `collection`;
-- reverse: create "characters" table
DROP TABLE `characters`;
-- reverse: create index "castentry_performer_id" to table: "cast_entries"
DROP INDEX `castentry_performer_id`;
-- reverse: create index "castentry_recording_id" to table: "cast_entries"
DROP INDEX `castentry_recording_id`;
-- reverse: create "cast_entries" table
DROP TABLE `cast_entries`;
