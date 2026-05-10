-- +goose Up
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_manual_import_queue" table
CREATE TABLE `new_manual_import_queue` (`id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `file_path` text NOT NULL, `file_size_bytes` integer NOT NULL DEFAULT (0), `discovered_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `suggested_recording_id` integer NULL, `suggested_confidence` text NOT NULL DEFAULT (''), `notes` text NOT NULL DEFAULT (''), `extras_count` integer NOT NULL DEFAULT (0));
-- copy rows from old table "manual_import_queue" to new temporary table "new_manual_import_queue"
INSERT INTO `new_manual_import_queue` (`id`, `file_path`, `file_size_bytes`, `discovered_at`, `last_seen_at`, `suggested_recording_id`, `suggested_confidence`, `notes`) SELECT `id`, `file_path`, `file_size_bytes`, `discovered_at`, `last_seen_at`, `suggested_recording_id`, `suggested_confidence`, `notes` FROM `manual_import_queue`;
-- drop "manual_import_queue" table after copying rows
DROP TABLE `manual_import_queue`;
-- rename temporary table "new_manual_import_queue" to "manual_import_queue"
ALTER TABLE `new_manual_import_queue` RENAME TO `manual_import_queue`;
-- create index "manual_import_queue_file_path_key" to table: "manual_import_queue"
CREATE UNIQUE INDEX `manual_import_queue_file_path_key` ON `manual_import_queue` (`file_path`);
-- create index "manualimportqueue_discovered_at" to table: "manual_import_queue"
CREATE INDEX `manualimportqueue_discovered_at` ON `manual_import_queue` (`discovered_at`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- reverse: create index "manualimportqueue_discovered_at" to table: "manual_import_queue"
DROP INDEX `manualimportqueue_discovered_at`;
-- reverse: create index "manual_import_queue_file_path_key" to table: "manual_import_queue"
DROP INDEX `manual_import_queue_file_path_key`;
-- reverse: create "new_manual_import_queue" table
DROP TABLE `new_manual_import_queue`;
