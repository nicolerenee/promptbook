package storage

// reconcile_files.go — re-discover on-disk state for a recording's
// versions. Driven by the "Refresh recording" button on the detail
// page when an external tool (Radarr/Plex) has changed the file's
// name on disk without telling promptbook. The scanner's
// externally-managed drift detector matches by basename, which
// breaks when Radarr replaces "Foo.mkv" with "Foo.2160p.mkv" as
// part of an upgrade. This pass is more aggressive: it walks each
// version's parent folder, pairs surviving rows with surviving
// files by full path first, and reassigns the unmatched leftovers
// by index when the count of stale rows equals the count of
// new-on-disk files.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nicolerenee/promptbook/internal/ent"
)

// videoExtensionsForReconcile mirrors ingest.VideoExtensions but is
// duplicated here to avoid the import cycle (ingest depends on
// storage). Kept in sync by hand — the set rarely changes.
//
//nolint:gochecknoglobals // immutable lookup table.
var videoExtensionsForReconcile = map[string]struct{}{
	".mkv": {}, ".mp4": {}, ".m4v": {}, ".avi": {},
	".mov": {}, ".ts": {}, ".mts": {}, ".m2ts": {},
	".wmv": {}, ".webm": {}, ".vob": {}, ".flv": {},
}

// FileReconcileChange describes one path swap applied by the
// reconciler. Returned so the caller (refresh-recording-full job)
// can log the diff and the SPA's success toast can name the file
// that changed.
type FileReconcileChange struct {
	VersionID int64
	OldPath   string
	NewPath   string
}

// ReconcileRecordingFiles walks each version's parent folder and
// updates recording_versions.file_path when an external tool has
// renamed the underlying file in place. Returns the list of
// (version_id, old_path, new_path) tuples that were updated.
//
// Pairing rule per folder:
//
//   - Version rows whose FilePath still exists on disk are
//     considered fresh — no change.
//   - Stale rows (FilePath now ENOENT) are paired against video
//     files in the same folder that no other version row points at.
//     When the stale count equals the unmatched-file count and is
//     unambiguous, each stale row is reassigned to the unmatched
//     file at the same index (sorted by basename for determinism).
//   - When counts don't match (file went missing entirely, or a
//     new file appeared without an old one being replaced), the
//     reconciler leaves those rows alone — the user resolves the
//     ambiguity via the queue / picker UI.
//
// Safe to call repeatedly; no-op when every version's FilePath is
// still on disk.
func ReconcileRecordingFiles(
	ctx context.Context, client *ent.Client, recordingID int64,
) ([]FileReconcileChange, error) {
	versions, err := ListVersions(ctx, client, recordingID)
	if err != nil {
		return nil, fmt.Errorf("list versions for recording %d: %w", recordingID, err)
	}
	if len(versions) == 0 {
		return nil, nil
	}

	// Group versions by parent folder so each folder's reconciliation
	// happens independently (multipart recordings can span folders if
	// the user organizes their library that way — rare, but the
	// per-folder pass keeps the logic local either way).
	byFolder := make(map[string][]RecordingVersion)
	for _, v := range versions {
		dir := filepath.Dir(v.FilePath)
		byFolder[dir] = append(byFolder[dir], v)
	}

	// Deterministic folder order so the change log reads stably across
	// runs.
	folders := make([]string, 0, len(byFolder))
	for f := range byFolder {
		folders = append(folders, f)
	}
	sort.Strings(folders)

	var changes []FileReconcileChange
	for _, folder := range folders {
		folderChanges, ferr := reconcileFolderVersions(
			ctx, client, folder, byFolder[folder])
		if ferr != nil {
			return changes, ferr
		}
		changes = append(changes, folderChanges...)
	}
	return changes, nil
}

// reconcileFolderVersions performs the per-folder pair-up. Pulled
// out of ReconcileRecordingFiles so each folder's stale/new sets
// stay self-contained and the funlen lint stays under cap.
func reconcileFolderVersions(
	ctx context.Context, client *ent.Client, folder string, versions []RecordingVersion,
) ([]FileReconcileChange, error) {
	freshPaths, staleVersions := partitionVersionsByExistence(versions)
	if len(staleVersions) == 0 {
		return nil, nil
	}

	onDisk, err := listVideoFilesInFolder(folder)
	if err != nil {
		// Folder is gone entirely — the recording's files are missing
		// at this location. Don't error; the caller's "Refresh"
		// surface should let the user reassign via the queue.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list videos in %s: %w", folder, err)
	}
	unmatched := subtractFresh(onDisk, freshPaths)
	if len(unmatched) != len(staleVersions) {
		// Ambiguous: counts don't line up. A file went missing
		// entirely, or a new file appeared without an old one being
		// replaced. Leave the stale rows pointing at their dead
		// paths so the recording surfaces as Missing — the user
		// sees the problem and can re-import via the queue.
		return nil, nil
	}

	// Sort both lists for stable pairing. Stale versions ordered by
	// existing basename; unmatched files ordered by their on-disk
	// basename. This gives deterministic pairings when both halves
	// of a multipart-upgrade rename share a common ordering convention
	// (act-1, act-2 → upgraded to Foo.S01.act-1, Foo.S01.act-2).
	sort.SliceStable(staleVersions, func(i, j int) bool {
		return filepath.Base(staleVersions[i].FilePath) <
			filepath.Base(staleVersions[j].FilePath)
	})
	sort.Strings(unmatched)

	changes := make([]FileReconcileChange, 0, len(staleVersions))
	for i, stale := range staleVersions {
		newPath := unmatched[i]
		if uerr := UpdateVersionFilePath(ctx, client, stale.ID, newPath); uerr != nil {
			return changes, fmt.Errorf(
				"update file_path for version %d: %w", stale.ID, uerr)
		}
		changes = append(changes, FileReconcileChange{
			VersionID: stale.ID,
			OldPath:   stale.FilePath,
			NewPath:   newPath,
		})
	}
	return changes, nil
}

// partitionVersionsByExistence splits versions into (still-on-disk
// paths, stale rows). The fresh paths are returned as a set so the
// caller can compute "files on disk that no version row claims" with
// a single pass.
func partitionVersionsByExistence(
	versions []RecordingVersion,
) (map[string]struct{}, []RecordingVersion) {
	fresh := make(map[string]struct{})
	stale := make([]RecordingVersion, 0)
	for _, v := range versions {
		if _, err := os.Stat(v.FilePath); err == nil {
			fresh[v.FilePath] = struct{}{}
			continue
		}
		stale = append(stale, v)
	}
	return fresh, stale
}

// listVideoFilesInFolder returns the absolute paths of every video
// file at the root of folder (no recursion — recursion would conflate
// per-track audio rips and other extras with the main file pool).
func listVideoFilesInFolder(folder string) ([]string, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if strings.HasPrefix(base, ".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(base))
		if _, ok := videoExtensionsForReconcile[ext]; !ok {
			continue
		}
		out = append(out, filepath.Join(folder, base))
	}
	return out, nil
}

// subtractFresh returns the entries of onDisk that aren't already
// claimed by a fresh version row.
func subtractFresh(onDisk []string, fresh map[string]struct{}) []string {
	out := make([]string, 0, len(onDisk))
	for _, p := range onDisk {
		if _, ok := fresh[p]; ok {
			continue
		}
		out = append(out, p)
	}
	return out
}
