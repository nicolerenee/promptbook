package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// Assignment kind tokens. The wire-format strings the GraphQL
// importQueueEntry mutation (and CLI, eventually) speak.
const (
	AssignmentKindMain        = "main"
	AssignmentKindSkip        = "skip"
	assignmentKindPrefixPart  = "part-"
	assignmentKindPrefixExtra = "extra-"
)

// AssignmentKindPart returns the assignment kind token for a part
// at the given 1-based index, e.g. AssignmentKindPart(2) → "part-2".
// Used by the scanner's classifier so the wire-format prefix lives
// in one place.
func AssignmentKindPart(index int) string {
	return fmt.Sprintf("%s%d", assignmentKindPrefixPart, index)
}

// AssignmentKindExtra returns the assignment kind token for an extra
// of the given kind, e.g. AssignmentKindExtra(ExtraKindFeaturette) →
// "extra-featurette". Used by the scanner's classifier so the wire-
// format prefix lives in one place.
func AssignmentKindExtra(kind string) string {
	return assignmentKindPrefixExtra + kind
}

// Extras kind tokens — what the queue modal labels each extra-kind
// dropdown option as. Mirrors Jellyfin's extras subfolder vocabulary
// (featurettes/scenes/...) plus two promptbook-only kinds (audio,
// photo) for content Jellyfin doesn't natively surface.
const (
	ExtraKindFeaturette      = "featurette"
	ExtraKindScene           = "scene"
	ExtraKindBehindTheScenes = "behindthescenes"
	ExtraKindInterview       = "interview"
	ExtraKindTrailer         = "trailer"
	ExtraKindDeletedScenes   = "deletedscenes"
	ExtraKindOther           = "other"
	ExtraKindAudio           = "audio"
	ExtraKindPhoto           = "photo"
)

// extraKindToSubfolder maps an extras kind to the canonical
// subfolder Jellyfin auto-discovers (or, for audio/photo, the
// promptbook convention). Anything not in the table falls through
// to ExtraKindOther's `other/` folder.
//
//nolint:gochecknoglobals // immutable lookup table.
var extraKindToSubfolder = map[string]string{
	ExtraKindFeaturette:      "featurettes",
	ExtraKindScene:           "scenes",
	ExtraKindBehindTheScenes: "behindthescenes",
	ExtraKindInterview:       "interviews",
	ExtraKindTrailer:         "trailers",
	ExtraKindDeletedScenes:   "deletedscenes",
	ExtraKindOther:           "other",
	ExtraKindAudio:           "audio",
	ExtraKindPhoto:           "photos",
}

// classifiedAssignments groups one ingest call's assignments by
// role. The shape mirrors the per-recording outcome: a single main
// (or N parts), zero-or-more extras, plus skipped paths the engine
// must leave alone.
type classifiedAssignments struct {
	main    *FileAssignment
	parts   []FileAssignment // sorted by part index ascending.
	extras  []FileAssignment
	skipped []FileAssignment
}

// classifyAssignments parses the kind tokens and bucketizes the
// assignments. Returns an error when the shape is invalid (no main
// AND no parts; both main and parts present; non-contiguous parts;
// duplicate part indices).
func classifyAssignments(in []FileAssignment) (*classifiedAssignments, error) {
	out := &classifiedAssignments{}
	partsByIndex := map[int]FileAssignment{}
	for _, a := range in {
		if err := classifyOne(a, out, partsByIndex); err != nil {
			return nil, err
		}
	}
	if err := validateAssignments(out, partsByIndex); err != nil {
		return nil, err
	}
	indices := make([]int, 0, len(partsByIndex))
	for idx := range partsByIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		out.parts = append(out.parts, partsByIndex[idx])
	}
	return out, nil
}

// classifyOne routes a single assignment into the right bucket on
// out / partsByIndex. Pulled out of classifyAssignments so the loop
// body stays under the gocognit threshold.
func classifyOne(
	a FileAssignment, out *classifiedAssignments, partsByIndex map[int]FileAssignment,
) error {
	switch {
	case a.Kind == AssignmentKindMain:
		if out.main != nil {
			return fmt.Errorf(
				"ingest: multiple main assignments (got %s and %s)",
				out.main.SourcePath, a.SourcePath)
		}
		a := a
		out.main = &a
		return nil
	case strings.HasPrefix(a.Kind, assignmentKindPrefixPart):
		idxStr := strings.TrimPrefix(a.Kind, assignmentKindPrefixPart)
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 1 {
			return fmt.Errorf(
				"ingest: invalid part kind %q on %s", a.Kind, a.SourcePath)
		}
		if existing, dup := partsByIndex[idx]; dup {
			return fmt.Errorf(
				"ingest: duplicate part-%d assignments (%s and %s)",
				idx, existing.SourcePath, a.SourcePath)
		}
		partsByIndex[idx] = a
		return nil
	case strings.HasPrefix(a.Kind, assignmentKindPrefixExtra):
		out.extras = append(out.extras, a)
		return nil
	case a.Kind == AssignmentKindSkip:
		out.skipped = append(out.skipped, a)
		return nil
	default:
		return fmt.Errorf(
			"ingest: unknown assignment kind %q on %s", a.Kind, a.SourcePath)
	}
}

// validateAssignments enforces the per-batch shape rules: exactly
// one of (main, parts) is present, parts are contiguous from 1,
// single-part-only is rejected (caller should use kind=main).
func validateAssignments(
	out *classifiedAssignments, partsByIndex map[int]FileAssignment,
) error {
	if out.main != nil && len(partsByIndex) > 0 {
		return errors.New(
			"ingest: assignments may have either a main or parts, not both")
	}
	if out.main == nil && len(partsByIndex) == 0 {
		return errors.New(
			"ingest: assignments must include a main or at least one part")
	}
	if len(partsByIndex) == 1 {
		return errors.New(
			"ingest: a single part-N assignment is not multipart; use kind=main instead")
	}
	return validateContiguous(partsByIndex)
}

// validateContiguous ensures part indices are 1..N with no gaps so
// the imported version's part_index column is dense.
func validateContiguous(byIdx map[int]FileAssignment) error {
	for i := 1; i <= len(byIdx); i++ {
		if _, ok := byIdx[i]; !ok {
			return fmt.Errorf(
				"ingest: part-%d missing — parts must be contiguous starting at 1", i)
		}
	}
	return nil
}

// ingestWithAssignments is the multi-file entry point. src is the
// containing folder (used for the recording_versions.source_folder
// stamp + the empty-folder cleanup); each assignment names a file
// inside that folder by absolute path.
//
// Behaviour:
//
//   - main / part-N assignments run through the existing single-file
//     pipeline (resolve id, lookup, plan, apply, NFO). The first
//     successful main / part-1 carries the AppliedExtras list back to
//     the caller.
//   - extra-{kind} assignments move into a Jellyfin-shaped subfolder
//     under the canonical recording folder and write a
//     recording_extras row each. Per-extra failures log + continue
//     so a single permission blip doesn't abort the batch.
//   - skip assignments are no-ops (the file stays in src).
//
// A failure on the main / part files aborts the call before any
// extras run — extras need the canonical folder, which only exists
// after the main move. Returns the per-file results in
// (mains/parts, then extras-as-skipped-rows order on a main failure)
// shape; the caller iterates Result.Items to render outcomes.
func (e *Engine) ingestWithAssignments(
	ctx context.Context, src string, opts Options,
) (*Result, error) {
	cls, err := classifyAssignments(opts.FileAssignments)
	if err != nil {
		return nil, err
	}
	res := &Result{}

	// Capture the source folder once: every main/part/extra inside
	// this batch lives under the same parent in real-world usage
	// (the queue modal scopes assignments to one folder), and the
	// version row's source_folder column tracks that parent so the
	// recording detail page can flag this as a folder-as-unit drop.
	sourceFolder := opts.SourceFolder
	if sourceFolder == "" {
		sourceFolder = filepath.Clean(src)
	}

	// Sub-options for the main/part calls: clear FileAssignments so
	// the recursive Ingest doesn't loop, carry the SourceFolder so
	// the version row gets stamped.
	subOpts := opts
	subOpts.FileAssignments = nil
	subOpts.SourceFolder = sourceFolder

	mains := e.ingestMainsAndParts(ctx, cls, subOpts, res)
	if mains.aborted {
		return res, nil
	}

	// Extras hang off the canonical recording folder produced by the
	// first main / part-1 plan.
	if mains.firstPlanFolder == "" {
		// Defensive: every successful main/part returns a plan with
		// a non-empty AbsoluteFolder. If we got here without one,
		// something upstream short-circuited; skip extras silently
		// rather than panic.
		return res, nil
	}
	e.applyExtras(ctx, cls.extras, mains, opts.DryRun)
	return res, nil
}

// mainsApplyOutcome carries the per-batch state the extras pass
// needs: the canonical folder, the recording id, and a pointer to
// the result row that owns AppliedExtras.
type mainsApplyOutcome struct {
	aborted           bool
	firstPlanFolder   string
	recordingID       int64
	appliedExtrasRow  *ItemResult
	externallyManaged bool
}

// ingestMainsAndParts runs every main / part assignment through the
// single-file pipeline. The first successful row is the "owner" of
// any extras applied later — its AppliedExtras list is the
// surface the GraphQL response renders against. Returns aborted=true
// when the main / part files all failed; the caller skips extras in
// that case (the canonical folder may not exist).
func (e *Engine) ingestMainsAndParts(
	ctx context.Context, cls *classifiedAssignments,
	opts Options, res *Result,
) mainsApplyOutcome {
	out := mainsApplyOutcome{externallyManaged: opts.ExternallyManaged}
	if cls.main != nil {
		seed := ItemResult{SourceFolder: opts.SourceFolder}
		item := e.ingestOneWithSeed(ctx, cls.main.SourcePath, opts, seed)
		res.Items = append(res.Items, item)
		if isAppliedOK(item) {
			out.firstPlanFolder = item.Plan.AbsoluteFolder()
			out.recordingID = item.EncoraID
			out.appliedExtrasRow = &res.Items[len(res.Items)-1]
		} else {
			out.aborted = true
		}
		return out
	}
	for i, p := range cls.parts {
		seed := ItemResult{
			SourceFolder: opts.SourceFolder,
			PartIndex:    i + 1,
		}
		item := e.ingestOneWithSeed(ctx, p.SourcePath, opts, seed)
		res.Items = append(res.Items, item)
		if !isAppliedOK(item) {
			out.aborted = true
			return out
		}
		if i == 0 {
			out.firstPlanFolder = item.Plan.AbsoluteFolder()
			out.recordingID = item.EncoraID
			out.appliedExtrasRow = &res.Items[len(res.Items)-1]
		}
	}
	return out
}

// isAppliedOK reports whether the item's pipeline finished without
// fatal errors. ActionMoved / ActionWouldMove (dry-run) are both
// considered success states for the extras-can-proceed gate.
func isAppliedOK(item ItemResult) bool {
	if item.Err != nil {
		return false
	}
	return item.Action == ActionMoved || item.Action == ActionWouldMove
}

// applyExtras walks the extras list, moves each into a Jellyfin-
// shaped subfolder under the canonical recording folder, and writes
// a recording_extras row per file moved. Per-extra failures append
// to the AppliedExtras list with Err populated; the engine keeps
// going so a single permission blip doesn't abort the batch.
//
// On dry-run, applyExtras records the planned destination on each
// AppliedExtra without touching the disk or the DB.
func (e *Engine) applyExtras(
	ctx context.Context, extras []FileAssignment,
	mains mainsApplyOutcome, dryRun bool,
) {
	if mains.appliedExtrasRow == nil || len(extras) == 0 {
		return
	}
	for _, ex := range extras {
		applied := e.applyOneExtra(ctx, ex, mains, dryRun)
		mains.appliedExtrasRow.AppliedExtras = append(
			mains.appliedExtrasRow.AppliedExtras, applied)
	}
}

// applyOneExtra moves a single extra file into its canonical
// subfolder + writes the recording_extras row. Returns an
// AppliedExtra tagged with Err on failure.
//
// For externally-managed imports the source file stays put — the
// recording_extras row records the file at its source path so the
// detail page can still surface the bonus material, but no copy /
// move runs. This matches the catalog-only behaviour the main file
// applies above.
func (e *Engine) applyOneExtra(
	ctx context.Context, ex FileAssignment,
	mains mainsApplyOutcome, dryRun bool,
) AppliedExtra {
	kind := strings.TrimPrefix(ex.Kind, assignmentKindPrefixExtra)
	if _, ok := extraKindToSubfolder[kind]; !ok {
		// Unknown kind: dump to other/ rather than failing. The user
		// can re-classify after the fact via a future admin surface;
		// phase 1 just needs the file landed.
		kind = ExtraKindOther
	}
	if mains.externallyManaged {
		return e.applyOneExtraExternallyManaged(ctx, ex, mains, kind, dryRun)
	}
	subfolder := extraKindToSubfolder[kind]
	destDir := filepath.Join(mains.firstPlanFolder, subfolder)
	dest := filepath.Join(destDir, filepath.Base(ex.SourcePath))
	out := AppliedExtra{
		SourcePath: ex.SourcePath,
		DestPath:   dest,
		Kind:       kind,
		Label:      ex.Label,
	}
	if dryRun {
		return out
	}

	if err := os.MkdirAll(destDir, libraryDirPerm); err != nil {
		out.Err = fmt.Errorf("mkdir extras subfolder: %w", err)
		e.Logger.Warn().Err(out.Err).Str("dir", destDir).
			Msg("failed to create extras subfolder")
		return out
	}
	size, moveErr := moveExtra(ex.SourcePath, dest)
	if moveErr != nil {
		out.Err = fmt.Errorf("move extra %s: %w", ex.SourcePath, moveErr)
		e.Logger.Warn().Err(out.Err).Str("source", ex.SourcePath).
			Str("dest", dest).Msg("failed to move extra")
		return out
	}

	row := storage.RecordingExtra{
		RecordingID:   mains.recordingID,
		FilePath:      dest,
		Kind:          kind,
		Label:         ex.Label,
		FileSizeBytes: size,
	}
	if _, err := storage.UpsertExtra(ctx, e.DB, row); err != nil {
		// File is in place; the row write is best-effort. Log + keep
		// going so the user can still see the file exists.
		out.Err = fmt.Errorf("upsert recording_extras row: %w", err)
		e.Logger.Warn().Err(out.Err).Str("dest", dest).
			Msg("failed to record extras row")
		return out
	}
	return out
}

// applyOneExtraExternallyManaged handles the catalog-only path for an
// extras assignment: leave the file at the source path, persist a
// recording_extras row that points at the source path, and stop. No
// directory creation, no move, no overwrite check — the external
// tool owns the file layout.
func (e *Engine) applyOneExtraExternallyManaged(
	ctx context.Context, ex FileAssignment,
	mains mainsApplyOutcome, kind string, dryRun bool,
) AppliedExtra {
	out := AppliedExtra{
		SourcePath: ex.SourcePath,
		DestPath:   ex.SourcePath,
		Kind:       kind,
		Label:      ex.Label,
	}
	if dryRun {
		return out
	}
	info, statErr := os.Stat(ex.SourcePath)
	if statErr != nil {
		out.Err = fmt.Errorf("stat extra %s: %w", ex.SourcePath, statErr)
		e.Logger.Warn().Err(out.Err).Str("source", ex.SourcePath).
			Msg("failed to stat externally-managed extra")
		return out
	}
	size := info.Size()
	if info.IsDir() {
		dirSize, dirErr := directorySize(ex.SourcePath)
		if dirErr != nil {
			out.Err = fmt.Errorf("size of extra dir %s: %w", ex.SourcePath, dirErr)
			e.Logger.Warn().Err(out.Err).Str("source", ex.SourcePath).
				Msg("failed to size externally-managed extra dir")
			return out
		}
		size = dirSize
	}
	row := storage.RecordingExtra{
		RecordingID:   mains.recordingID,
		FilePath:      ex.SourcePath,
		Kind:          kind,
		Label:         ex.Label,
		FileSizeBytes: size,
	}
	if _, err := storage.UpsertExtra(ctx, e.DB, row); err != nil {
		out.Err = fmt.Errorf(
			"upsert externally-managed recording_extras row: %w", err)
		e.Logger.Warn().Err(out.Err).Str("path", ex.SourcePath).
			Msg("failed to record externally-managed extras row")
		return out
	}
	return out
}

// libraryDirPerm matches rename.libraryDirPerm — kept here to avoid
// exposing the rename package's unexported constant.
const libraryDirPerm = 0o755

// libraryFilePerm matches rename.libraryFilePerm; world-readable so
// downstream Jellyfin / Plex containers running as a different uid
// can read the moved files.
const libraryFilePerm = 0o644

// moveExtra renames or copy+removes a file or directory from src to
// dest. Returns the cumulative size of the moved content (for
// directories, the sum of every file inside).
func moveExtra(src, dest string) (int64, error) {
	info, statErr := os.Stat(src)
	if statErr != nil {
		return 0, fmt.Errorf("stat source: %w", statErr)
	}
	if info.IsDir() {
		size, dirErr := moveDirectory(src, dest)
		if dirErr != nil {
			return 0, dirErr
		}
		return size, nil
	}
	if mvErr := moveFile(src, dest); mvErr != nil {
		return 0, mvErr
	}
	return info.Size(), nil
}

// moveFile is the per-file mover used by both the file and directory
// paths. Rename first, fall back to copy+remove on cross-device
// errors so /incoming → /library mounts on different filesystems
// still succeed.
func moveFile(src, dest string) error {
	if renameErr := os.Rename(src, dest); renameErr == nil {
		return nil
	} else if !isCrossDeviceMoveErr(renameErr) {
		return fmt.Errorf("rename: %w", renameErr)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, libraryFilePerm)
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		return fmt.Errorf("close dest: %w", closeErr)
	}
	if rmErr := os.Remove(src); rmErr != nil {
		return fmt.Errorf("remove source after copy: %w", rmErr)
	}
	return nil
}

// moveDirectory tries os.Rename for the whole tree first; on
// cross-device errors it falls back to a recursive copy + remove.
// Returns the cumulative byte size of the moved content.
func moveDirectory(src, dest string) (int64, error) {
	if err := os.Rename(src, dest); err == nil {
		return directorySize(dest)
	} else if !isCrossDeviceMoveErr(err) {
		return 0, fmt.Errorf("rename directory: %w", err)
	}
	if err := copyDirectory(src, dest); err != nil {
		return 0, err
	}
	if err := os.RemoveAll(src); err != nil {
		return 0, fmt.Errorf("remove source dir after copy: %w", err)
	}
	return directorySize(dest)
}

// copyDirectory walks src and rebuilds the same shape under dest.
// Best-effort — partial failures bubble up.
func copyDirectory(src, dest string) error {
	walkErr := filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			if mkErr := os.MkdirAll(target, libraryDirPerm); mkErr != nil {
				return fmt.Errorf("mkdir %s: %w", target, mkErr)
			}
			return nil
		}
		return copyFile(path, target)
	})
	if walkErr != nil {
		return fmt.Errorf("copy dir: %w", walkErr)
	}
	return nil
}

// copyFile streams from src to dest with the canonical world-
// readable permission so downstream consumers can read it.
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, libraryFilePerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy %s -> %s: %w", src, dest, copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		return fmt.Errorf("close %s: %w", dest, closeErr)
	}
	return nil
}

// directorySize sums the byte size of every regular file under
// path. Used to populate recording_extras.file_size_bytes for
// directory-shaped extras (the Halcyon Crossing audio/ subfolder case).
func directorySize(path string) (int64, error) {
	var total int64
	walkErr := filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("walk %s: %w", path, walkErr)
	}
	return total, nil
}

// isCrossDeviceMoveErr recognizes EXDEV from os.Rename. Mirrors
// rename.isCrossDevice; duplicated here so the assignments path
// doesn't import the rename internals.
func isCrossDeviceMoveErr(err error) bool {
	if err == nil {
		return false
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || linkErr.Err == nil {
		return false
	}
	msg := linkErr.Err.Error()
	return strings.Contains(msg, "cross-device") || strings.Contains(msg, "EXDEV")
}
