-- +goose Up
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_recordings" table
CREATE TABLE `new_recordings` (`recording_id` integer NOT NULL PRIMARY KEY AUTOINCREMENT, `tour` text NOT NULL DEFAULT (''), `date_full` text NOT NULL DEFAULT (''), `date_month_known` bool NOT NULL DEFAULT (false), `date_day_known` bool NOT NULL DEFAULT (false), `date_variant` text NULL, `date_time` text NOT NULL DEFAULT ('unknown'), `master` text NOT NULL DEFAULT (''), `nft_date` text NULL, `nft_forever` bool NOT NULL DEFAULT (false), `notes` text NOT NULL DEFAULT (''), `master_notes` text NULL, `release_format` text NULL, `venue` text NOT NULL DEFAULT (''), `city` text NOT NULL DEFAULT (''), `media_type` text NOT NULL DEFAULT (''), `recording_type` text NOT NULL DEFAULT (''), `amount_recorded` text NOT NULL DEFAULT (''), `gifting_status` text NOT NULL DEFAULT (''), `limited_status` text NOT NULL DEFAULT (''), `is_opening` bool NOT NULL DEFAULT (false), `is_closing` bool NOT NULL DEFAULT (false), `is_preview` bool NOT NULL DEFAULT (false), `is_concert` bool NOT NULL DEFAULT (false), `is_nfs` bool NOT NULL DEFAULT (false), `is_favourite` bool NOT NULL DEFAULT (false), `has_screenshots` bool NOT NULL DEFAULT (false), `has_subtitles` bool NOT NULL DEFAULT (false), `boot_camp_recommended` bool NOT NULL DEFAULT (false), `owners_count` integer NOT NULL DEFAULT (0), `wanters_count` integer NOT NULL DEFAULT (0), `externally_managed` bool NOT NULL DEFAULT (false), `last_updated` text NOT NULL DEFAULT (''), `raw_json` text NOT NULL, `last_seen_at` datetime NOT NULL DEFAULT (CURRENT_TIMESTAMP), `show_id` integer NOT NULL, CONSTRAINT `recordings_shows_recordings` FOREIGN KEY (`show_id`) REFERENCES `shows` (`show_id`) ON DELETE NO ACTION);
-- copy rows from old table "recordings" to new temporary table "new_recordings"
INSERT INTO `new_recordings` (`recording_id`, `tour`, `date_full`, `date_month_known`, `date_day_known`, `date_variant`, `date_time`, `master`, `nft_date`, `nft_forever`, `notes`, `master_notes`, `release_format`, `venue`, `city`, `media_type`, `recording_type`, `amount_recorded`, `gifting_status`, `limited_status`, `is_opening`, `is_closing`, `is_preview`, `is_concert`, `is_nfs`, `is_favourite`, `has_screenshots`, `has_subtitles`, `boot_camp_recommended`, `owners_count`, `wanters_count`, `last_updated`, `raw_json`, `last_seen_at`, `show_id`) SELECT `recording_id`, `tour`, `date_full`, `date_month_known`, `date_day_known`, `date_variant`, `date_time`, `master`, `nft_date`, `nft_forever`, `notes`, `master_notes`, `release_format`, `venue`, `city`, `media_type`, `recording_type`, `amount_recorded`, `gifting_status`, `limited_status`, `is_opening`, `is_closing`, `is_preview`, `is_concert`, `is_nfs`, `is_favourite`, `has_screenshots`, `has_subtitles`, `boot_camp_recommended`, `owners_count`, `wanters_count`, `last_updated`, `raw_json`, `last_seen_at`, `show_id` FROM `recordings`;
-- drop "recordings" table after copying rows
DROP TABLE `recordings`;
-- rename temporary table "new_recordings" to "recordings"
ALTER TABLE `new_recordings` RENAME TO `recordings`;
-- create index "recording_show_id" to table: "recordings"
CREATE INDEX `recording_show_id` ON `recordings` (`show_id`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- reverse: create index "recording_show_id" to table: "recordings"
DROP INDEX `recording_show_id`;
-- reverse: create "new_recordings" table
DROP TABLE `new_recordings`;
