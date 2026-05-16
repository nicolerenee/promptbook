-- +goose Up
-- Add the private_notes field to recordings. User-owned free-text
-- the catalog never syncs to Encora — trade notes, "I owe Alex a
-- copy", subtitle quality reminders, etc. Empty string is the
-- documented "no notes" sentinel so the column is non-null.
--
-- Plain ALTER TABLE ADD COLUMN to avoid the SQLite-table-rebuild
-- cascade trap that wiped recording_versions on the externally_managed
-- migration. See internal/dbm/migrations/20260510184520_*.sql for the
-- post-mortem on why goose + PRAGMA foreign_keys=off don't mix.
ALTER TABLE `recordings` ADD COLUMN `private_notes` text NOT NULL DEFAULT ('');

-- +goose Down
ALTER TABLE `recordings` DROP COLUMN `private_notes`;
