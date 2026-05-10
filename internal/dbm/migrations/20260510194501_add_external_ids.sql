-- +goose Up
-- create "external_ids" table — one row per (recording_id, provider).
-- The table is intentionally NOT modeled in ent (see the note on
-- Recording.Edges); the externalids package reads/writes through raw
-- SQL on the ent driver. Composite PK (recording_id, provider) keeps
-- the per-recording-per-provider invariant at the storage layer; the
-- secondary UNIQUE INDEX on (provider, external_id) lets
-- FindRecordingByExternalID look up a local recording in O(1).
--
-- external_id is TEXT because IMDB ids are not numeric (tt99999999).
-- Encora ids round-trip through this column as the decimal string
-- form of the int64 (strconv.FormatInt) — see
-- internal/externalids/externalids.go for the encoding contract.
CREATE TABLE `external_ids` (
  `recording_id` integer NOT NULL,
  `provider` text NOT NULL,
  `external_id` text NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT (CURRENT_TIMESTAMP),
  PRIMARY KEY (`recording_id`, `provider`),
  CONSTRAINT `external_ids_recordings_external_ids` FOREIGN KEY (`recording_id`) REFERENCES `recordings` (`recording_id`) ON DELETE CASCADE
);
-- create unique index "external_ids_provider_external_id_idx" — used
-- by FindRecordingByExternalID to resolve a (provider, id) pair to
-- the local recording without scanning the table.
CREATE UNIQUE INDEX `external_ids_provider_external_id_idx` ON `external_ids` (`provider`, `external_id`);
-- back-fill: every existing recording gets its encora row. Phase L
-- will flip recordings.id to a locally-generated ulid, at which point
-- this is the only place the canonical Encora id lives. New code
-- already reads through the externalids package so the swap is a
-- single-table refactor rather than a 50-callsite rewrite.
INSERT INTO `external_ids` (`recording_id`, `provider`, `external_id`)
SELECT `recording_id`, 'encora', CAST(`recording_id` AS TEXT) FROM `recordings`;

-- +goose Down
-- reverse: drop unique index "external_ids_provider_external_id_idx"
DROP INDEX `external_ids_provider_external_id_idx`;
-- reverse: drop "external_ids" table
DROP TABLE `external_ids`;
