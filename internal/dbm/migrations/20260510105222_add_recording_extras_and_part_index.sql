-- +goose Up
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_recording_versions" table
CREATE TABLE `new_recording_versions` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `file_path` text NOT NULL, `file_size_bytes` integer NOT NULL DEFAULT (0), `container` text NOT NULL DEFAULT (''), `quality` text NOT NULL DEFAULT (''), `video_codec` text NOT NULL DEFAULT (''), `audio_codec` text NOT NULL DEFAULT (''), `format_label` text NOT NULL DEFAULT (''), `notes` text NOT NULL DEFAULT (''), `media_info_json` text NOT NULL DEFAULT (''), `source_folder` text NOT NULL DEFAULT (''), `part_index` integer NOT NULL DEFAULT (0), `added_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `recording_id` integer NOT NULL, CONSTRAINT `recording_versions_recordings_versions` FOREIGN KEY (`recording_id`) REFERENCES `recordings` (`recording_id`) ON DELETE CASCADE);
-- copy rows from old table "recording_versions" to new temporary table "new_recording_versions"
INSERT INTO `new_recording_versions` (`id`, `file_path`, `file_size_bytes`, `container`, `quality`, `video_codec`, `audio_codec`, `format_label`, `notes`, `media_info_json`, `source_folder`, `added_at`, `last_seen_at`, `recording_id`) SELECT `id`, `file_path`, `file_size_bytes`, `container`, `quality`, `video_codec`, `audio_codec`, `format_label`, `notes`, `media_info_json`, `source_folder`, `added_at`, `last_seen_at`, `recording_id` FROM `recording_versions`;
-- drop "recording_versions" table after copying rows
DROP TABLE `recording_versions`;
-- rename temporary table "new_recording_versions" to "recording_versions"
ALTER TABLE `new_recording_versions` RENAME TO `recording_versions`;
-- create index "recordingversion_recording_id" to table: "recording_versions"
CREATE INDEX `recordingversion_recording_id` ON `recording_versions` (`recording_id`);
-- create index "recordingversion_recording_id_file_path" to table: "recording_versions"
CREATE UNIQUE INDEX `recordingversion_recording_id_file_path` ON `recording_versions` (`recording_id`, `file_path`);
-- create "recording_extras" table
CREATE TABLE `recording_extras` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `file_path` text NOT NULL, `kind` text NOT NULL, `label` text NOT NULL DEFAULT (''), `file_size_bytes` integer NOT NULL DEFAULT (0), `added_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `recording_id` integer NOT NULL, CONSTRAINT `recording_extras_recordings_extras` FOREIGN KEY (`recording_id`) REFERENCES `recordings` (`recording_id`) ON DELETE CASCADE);
-- create index "extraentry_recording_id" to table: "recording_extras"
CREATE INDEX `extraentry_recording_id` ON `recording_extras` (`recording_id`);
-- create index "extraentry_recording_id_file_path" to table: "recording_extras"
CREATE UNIQUE INDEX `extraentry_recording_id_file_path` ON `recording_extras` (`recording_id`, `file_path`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- reverse: create index "extraentry_recording_id_file_path" to table: "recording_extras"
DROP INDEX `extraentry_recording_id_file_path`;
-- reverse: create index "extraentry_recording_id" to table: "recording_extras"
DROP INDEX `extraentry_recording_id`;
-- reverse: create "recording_extras" table
DROP TABLE `recording_extras`;
-- reverse: create index "recordingversion_recording_id_file_path" to table: "recording_versions"
DROP INDEX `recordingversion_recording_id_file_path`;
-- reverse: create index "recordingversion_recording_id" to table: "recording_versions"
DROP INDEX `recordingversion_recording_id`;
-- reverse: create "new_recording_versions" table
DROP TABLE `new_recording_versions`;
