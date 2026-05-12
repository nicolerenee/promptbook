package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/makemkv"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// jobNameRemuxDVD is the registry key. Stable string — surfaced in
// logs, job_runs.job_name, and the GraphQL mutation that fires it.
const jobNameRemuxDVD = "remux-dvd"

// argKeyTitleIndexes is the JobArgs key carrying the slice of
// makemkvcon title indexes the user selected in the picker modal.
const argKeyTitleIndexes = "title_indexes"

// originalSubfolder is the destination subfolder under the recording
// folder where the bit-for-bit VIDEO_TS originals are preserved.
// The user explicitly requires these stay on disk (trading expects
// the original DVD layout) — the .mkv conversion lives alongside,
// not in place of, the VOBs.
const originalSubfolder = "original"

// conversionNotesFilename is the audit file dropped into
// {recordingFolder}/original/ explaining what the remux did + the
// exact makemkvcon commands used. Mirrors the user's request:
// "this file was converted with promptbook using makemkv and how
// we didn't transcode it or anything but left it exactly bit for
// bit and show the exact command we ran".
const conversionNotesFilename = "CONVERSION_NOTES.txt"

// remuxFilePerm matches the rest of the ingest pipeline — world
// readable so the user's Jellyfin container (different uid) can
// read the produced .mkv + notes file without further chown.
const remuxFilePerm = 0o644

// remuxDirPerm matches mover.libraryDirPerm — world readable +
// traversable so downstream consumers (Jellyfin, the user's
// filesystem browser) can list the new original/ subdirectory.
const remuxDirPerm = 0o755

// RemuxDVDJob is the long-running async job kicked off by the SPA's
// "Remux to MKV (lossless)" affordance. It is INTENTIONALLY manual-
// only — the work takes 5–15 minutes for a typical 4 GB DVD and
// shouldn't fire on a recurring cadence.
//
// The pipeline, in order:
//
//  1. Validate: recording exists, library + makemkv configured,
//     recording has a /VIDEO_TS/ path among its versions.
//  2. Identify recordingFolder (parent of VIDEO_TS). Compute
//     originalDir := {recordingFolder}/original/. Move VIDEO_TS into
//     originalDir (idempotent — partial prior runs reuse).
//  3. Create a temp outDir. For each selected title index run
//     makemkv.Client.Mkv against originalDir. Diff the directory to
//     identify the produced .mkv.
//  4. Probe each .mkv with the configured Prober; build a rename
//     plan to derive the canonical filename. Force the destination
//     folder to the existing recordingFolder (the recording is
//     already canonical on disk).
//  5. Move each produced .mkv to recordingFolder under its
//     canonical filename.
//  6. Delete every existing recording_versions row for this
//     recording (the old VOB rows), then upsert one row per
//     produced .mkv with PartIndex 1..N.
//  7. Write originalDir/CONVERSION_NOTES.txt with the exact
//     makemkvcon commands run + the produced filenames.
//  8. Call NFORefresh so movie.nfo reflects the new format string.
//
// All downstream dependencies degrade independently:
//   - MakeMKV nil → job fails fast with a typed error so the SPA
//     can surface "Configure library.makemkvPath".
//   - Prober nil → fallthrough zero MediaInfo; the rename template
//     still resolves but probe-derived tokens collapse cleanly.
//   - NFORefresh nil → step 8 is skipped (debug log only).
type RemuxDVDJob struct {
	DB             *DBConn
	MakeMKV        *makemkv.Client
	Prober         probe.Prober
	NFORefresh     *nforefresh.Service
	LibraryRoot    string
	FolderTemplate string
	FileTemplate   string
	// Now is the wall-clock source the CONVERSION_NOTES timestamp
	// reads from. nil falls back to time.Now; tests inject a fixed
	// clock so the notes content is deterministic.
	Now    func() time.Time
	Logger zerolog.Logger
}

// Name returns the registry key.
func (j *RemuxDVDJob) Name() string { return jobNameRemuxDVD }

// Run executes the remux pipeline. Args:
//
//	recording_id:  int64    (required, must be > 0)
//	title_indexes: []int    (required, non-empty)
//
// Returns an error only on validation failure / unrecoverable
// pipeline failure. Per-title makemkvcon errors abort the whole
// run (we don't want a partial recording_versions swap with some
// rows pointing at old VOBs and some at new MKVs).
func (j *RemuxDVDJob) Run(ctx context.Context, args jobs.JobArgs) error {
	recID := args.GetInt64(argKeyRecordingID)
	if recID <= 0 {
		return errors.New("remux-dvd: missing or invalid recording_id arg")
	}
	titleIndexes, err := parseTitleIndexes(args)
	if err != nil {
		return fmt.Errorf("remux-dvd: %w", err)
	}
	if len(titleIndexes) == 0 {
		return errors.New("remux-dvd: title_indexes must be non-empty")
	}
	if j.MakeMKV == nil {
		return errors.New("remux-dvd: makemkv client not configured")
	}
	if j.LibraryRoot == "" || j.FolderTemplate == "" || j.FileTemplate == "" {
		return errors.New("remux-dvd: library root + templates required")
	}
	if j.DB == nil {
		return errors.New("remux-dvd: db not configured")
	}

	loaded, err := storage.LoadRecording(ctx, j.DB, recID)
	if err != nil {
		return fmt.Errorf("remux-dvd: load recording %d: %w", recID, err)
	}
	recFolder, err := findRecordingDVDFolder(loaded.Versions)
	if err != nil {
		return fmt.Errorf("remux-dvd: recording %d: %w", recID, err)
	}

	originalDir := filepath.Join(recFolder, originalSubfolder)
	if mvErr := moveVIDEOTSToOriginal(recFolder, originalDir); mvErr != nil {
		return fmt.Errorf("remux-dvd: preserve originals: %w", mvErr)
	}

	tempOut, err := os.MkdirTemp("", "promptbook-remux-")
	if err != nil {
		return fmt.Errorf("remux-dvd: create temp output: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempOut) }()

	produced, commands, err := j.runMakeMKV(ctx, originalDir, titleIndexes, tempOut)
	if err != nil {
		return fmt.Errorf("remux-dvd: %w", err)
	}
	if len(produced) == 0 {
		return errors.New("remux-dvd: makemkvcon produced no .mkv output")
	}

	moved, err := j.moveProducedFiles(ctx, loaded, recFolder, produced)
	if err != nil {
		return fmt.Errorf("remux-dvd: move produced files: %w", err)
	}

	if dbErr := j.swapVersionRows(ctx, recID, moved); dbErr != nil {
		return fmt.Errorf("remux-dvd: swap version rows: %w", dbErr)
	}

	if writeErr := j.writeConversionNotes(originalDir, recID, commands, moved); writeErr != nil {
		j.Logger.Warn().Err(writeErr).
			Int64("recording_id", recID).
			Msg("remux-dvd: write conversion notes failed; continuing")
	}

	j.refreshNFO(ctx, recID)

	destPaths := make([]string, 0, len(moved))
	for _, m := range moved {
		destPaths = append(destPaths, m.DestPath)
	}
	j.Logger.Info().
		Int64("recording_id", recID).
		Int("titles", len(produced)).
		Strs("produced", destPaths).
		Msg("remux-dvd: pass complete")
	return nil
}

// parseTitleIndexes decodes the title_indexes arg from a JobArgs
// blob. JSON round-trip converts the slice values to float64, so we
// tolerate both float64 and int shapes for ease of testing.
func parseTitleIndexes(args jobs.JobArgs) ([]int, error) {
	raw, ok := args[argKeyTitleIndexes]
	if !ok {
		return nil, errors.New("missing title_indexes arg")
	}
	switch v := raw.(type) {
	case []int:
		out := make([]int, 0, len(v))
		for _, n := range v {
			if n < 0 {
				return nil, fmt.Errorf("negative title index %d", n)
			}
			out = append(out, n)
		}
		return out, nil
	case []any:
		out := make([]int, 0, len(v))
		for _, item := range v {
			n, convErr := toInt(item)
			if convErr != nil {
				return nil, convErr
			}
			if n < 0 {
				return nil, fmt.Errorf("negative title index %d", n)
			}
			out = append(out, n)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("title_indexes must be a list, got %T", raw)
	}
}

// toInt converts a JSON-decoded numeric value to int, tolerating
// float64 / int / int64 shapes. encoding/json always lands ints in
// float64 unless decoded into a typed slice.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case int32:
		return int(n), nil
	case float64:
		return int(n), nil
	case float32:
		return int(n), nil
	default:
		return 0, fmt.Errorf("title index has unsupported type %T", v)
	}
}

// findRecordingDVDFolder locates the recording folder by walking the
// version rows for one whose FilePath contains "/VIDEO_TS/" — the
// parent of that VIDEO_TS subfolder is the recording's canonical
// home. Returns an error when no version row carries a VIDEO_TS
// path (the recording isn't a DVD layout) or when the FilePath shape
// is unparseable.
func findRecordingDVDFolder(versions []storage.RecordingVersion) (string, error) {
	const marker = "/VIDEO_TS/"
	for _, v := range versions {
		idx := strings.Index(v.FilePath, marker)
		if idx < 0 {
			continue
		}
		folder := v.FilePath[:idx]
		if folder == "" {
			return "", fmt.Errorf("VIDEO_TS path %q has no parent folder", v.FilePath)
		}
		return folder, nil
	}
	return "", errors.New("no version row references a /VIDEO_TS/ path")
}

// moveVIDEOTSToOriginal moves recFolder/VIDEO_TS into
// recFolder/original/VIDEO_TS so the bit-for-bit DVD originals are
// preserved for trading. Idempotent: if originalDir/VIDEO_TS
// already exists, the move is skipped (a prior partial run already
// preserved the originals).
//
// Side effect: the recording folder briefly has no VIDEO_TS at the
// top level after this returns — the .mkv conversion lands at the
// top level so the canonical layout becomes recFolder/movie.mkv +
// recFolder/original/VIDEO_TS/. (Jellyfin scans the .mkv; the
// originals are an offline-only artifact.)
func moveVIDEOTSToOriginal(recFolder, originalDir string) error {
	srcVTS := filepath.Join(recFolder, "VIDEO_TS")
	destVTS := filepath.Join(originalDir, "VIDEO_TS")

	// Idempotency: a prior partial run may have already moved
	// VIDEO_TS into original/. In that case the source no longer
	// exists at the top level — nothing to do.
	if _, statErr := os.Stat(destVTS); statErr == nil {
		// Destination already has the originals. If the source ALSO
		// still exists (interrupted move?), bail rather than overwrite.
		if _, srcErr := os.Stat(srcVTS); srcErr == nil {
			return fmt.Errorf(
				"both %q and %q exist; refusing to overwrite preserved originals",
				srcVTS, destVTS)
		}
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat preserved originals: %w", statErr)
	}

	if _, srcErr := os.Stat(srcVTS); srcErr != nil {
		return fmt.Errorf("source VIDEO_TS missing: %w", srcErr)
	}

	if mkErr := os.MkdirAll(originalDir, remuxDirPerm); mkErr != nil {
		return fmt.Errorf("create original dir: %w", mkErr)
	}
	if renameErr := os.Rename(srcVTS, destVTS); renameErr != nil {
		return fmt.Errorf("move VIDEO_TS to original: %w", renameErr)
	}
	return nil
}

// runMakeMKV invokes makemkvcon once per selected title against the
// preserved originals folder, returning the paths to the produced
// .mkv files and the exact commands run (for the audit notes).
//
// The path discovery is a diff: before each invocation we list the
// tempOut contents, after we list again, and the new .mkv files are
// attributed to the title that just ran. makemkvcon doesn't tell us
// the output filename directly so this is the cleanest discovery
// approach without parsing more of the robot protocol.
func (j *RemuxDVDJob) runMakeMKV(
	ctx context.Context, originalDir string, titleIndexes []int, tempOut string,
) ([]string, []string, error) {
	produced := make([]string, 0, len(titleIndexes))
	commands := make([]string, 0, len(titleIndexes))

	for _, idx := range titleIndexes {
		before, err := listMKVFiles(tempOut)
		if err != nil {
			return nil, nil, fmt.Errorf("list output dir before title %d: %w", idx, err)
		}
		cmd := fmt.Sprintf(
			`makemkvcon --robot --noscan mkv file:%q %d %q`,
			originalDir, idx, tempOut,
		)
		j.Logger.Info().
			Str("command", cmd).
			Msg("remux-dvd: invoking makemkvcon")
		if mkErr := j.MakeMKV.Mkv(ctx, originalDir, idx, tempOut); mkErr != nil {
			return nil, nil, fmt.Errorf("makemkvcon title %d: %w", idx, mkErr)
		}
		after, err := listMKVFiles(tempOut)
		if err != nil {
			return nil, nil, fmt.Errorf("list output dir after title %d: %w", idx, err)
		}
		newFiles := diffMKVFiles(before, after)
		if len(newFiles) == 0 {
			return nil, nil, fmt.Errorf("title %d: makemkvcon produced no new .mkv file", idx)
		}
		produced = append(produced, newFiles...)
		commands = append(commands, cmd)
	}
	return produced, commands, nil
}

// listMKVFiles enumerates every *.mkv file at the top level of dir.
// Returns paths joined with dir. Subdirectories are not walked —
// makemkvcon lands every title at the top level of outDir.
func listMKVFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %q: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(e.Name()), ".mkv") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// diffMKVFiles returns paths in `after` that are not in `before`.
// Order is preserved from `after`. Used to attribute newly-produced
// .mkv files to the title invocation that just ran.
func diffMKVFiles(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, p := range before {
		seen[p] = struct{}{}
	}
	out := make([]string, 0, len(after))
	for _, p := range after {
		if _, ok := seen[p]; ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// movedRemux records one .mkv that was moved into the recording
// folder under its canonical filename. probedInfo is held for the
// post-move recording_versions upsert so we don't re-probe the
// file twice.
type movedRemux struct {
	SourceMKV   string
	DestPath    string
	PartIndex   int
	ProbedInfo  probe.MediaInfo
	FileSize    int64
	FormatLabel string
}

// moveProducedFiles probes each produced .mkv, builds a rename plan
// using the recording's metadata, and moves the file from tempOut
// into the recording's existing canonical folder. The destination
// folder is FORCED to recFolder rather than the template-rendered
// folder — the recording is already in canonical-folder shape on
// disk (movie.nfo + poster.jpg + original/), and moving it would
// orphan all that.
//
// Multi-title runs stamp PartIndex 1..N onto each row so the
// existing multipart format-string machinery groups them under one
// release-format display string.
func (j *RemuxDVDJob) moveProducedFiles(
	ctx context.Context,
	loaded *storage.LoadedRecording,
	recFolder string,
	produced []string,
) ([]movedRemux, error) {
	multipart := len(produced) > 1
	out := make([]movedRemux, 0, len(produced))
	for i, srcMKV := range produced {
		info := probe.MediaInfo{}
		if j.Prober != nil {
			probed, probeErr := j.Prober.Probe(ctx, srcMKV)
			if probeErr != nil {
				j.Logger.Warn().
					Err(probeErr).
					Str("path", srcMKV).
					Msg("remux-dvd: probe produced .mkv failed; continuing with empty MediaInfo")
			} else {
				info = probed
			}
		}
		part := 0
		if multipart {
			part = i + 1
		}
		plan, err := rename.BuildPlan(rename.PlanInputs{
			Recording:      loaded.Recording,
			Source:         srcMKV,
			LibraryRoot:    j.LibraryRoot,
			FolderTemplate: j.FolderTemplate,
			FileTemplate:   j.FileTemplate,
			MediaInfo:      info,
			Part:           part,
		})
		if err != nil {
			return nil, fmt.Errorf("build plan for title %d: %w", i, err)
		}
		// Force destination into the EXISTING recording folder.
		// Don't move the .mkv into a template-derived folder — the
		// recording is already in canonical shape, with movie.nfo
		// + poster.jpg + original/. Moving the .mkv elsewhere would
		// orphan all that.
		destBase := plan.TargetFile + plan.Extension
		destPath := filepath.Join(recFolder, destBase)
		if mvErr := moveFile(srcMKV, destPath); mvErr != nil {
			return nil, fmt.Errorf("move produced .mkv to %q: %w", destPath, mvErr)
		}
		stat, statErr := os.Stat(destPath)
		var size int64
		if statErr == nil {
			size = stat.Size()
		}
		out = append(out, movedRemux{
			SourceMKV:   srcMKV,
			DestPath:    destPath,
			PartIndex:   part,
			ProbedInfo:  info,
			FileSize:    size,
			FormatLabel: "lossless DVD remux",
		})
	}
	return out, nil
}

// moveFile renames src to dest. Falls through to copy+remove on
// cross-device errors (mover.go has a fancier version but exposing
// it would bloat the rename package's surface; tempOut → recording
// folder is the only cross-device hop the remux job hits in
// practice and even that's rare given temp lives in /tmp).
func moveFile(src, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination %q already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), remuxDirPerm); err != nil {
		return fmt.Errorf("mkdir dest folder: %w", err)
	}
	if renameErr := os.Rename(src, dest); renameErr == nil {
		return nil
	}
	// Fallback: copy + remove. Used for cross-device renames where
	// os.Rename returns EXDEV. The remuxFilePerm bit-for-bit
	// permission stays consistent with the rest of the pipeline.
	if copyErr := copyAndRemove(src, dest); copyErr != nil {
		return copyErr
	}
	return nil
}

// copyAndRemove streams src to dest then removes src. Pulled out of
// moveFile so the cross-device fallback branch stays compact.
func copyAndRemove(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source for copy: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, remuxFilePerm)
	if err != nil {
		return fmt.Errorf("open dest for copy: %w", err)
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy bytes: %w", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		return fmt.Errorf("close dest: %w", closeErr)
	}
	if rmErr := os.Remove(src); rmErr != nil {
		return fmt.Errorf("remove source after copy: %w", rmErr)
	}
	return nil
}

// swapVersionRows deletes every existing recording_versions row for
// the recording and inserts a fresh row per produced .mkv. The
// (recording_id, file_path) unique index forbids reusing the same
// path while the old row exists; the delete-all-then-insert-all
// shape is the safest way to avoid a partial overlap.
func (j *RemuxDVDJob) swapVersionRows(
	ctx context.Context, recordingID int64, moved []movedRemux,
) error {
	if err := storage.DeleteVersionsForRecording(ctx, j.DB, recordingID); err != nil {
		return fmt.Errorf("delete old versions: %w", err)
	}
	for _, m := range moved {
		blob, marshalErr := json.Marshal(m.ProbedInfo)
		if marshalErr != nil {
			return fmt.Errorf("marshal media info: %w", marshalErr)
		}
		v := storage.RecordingVersion{
			RecordingID:   recordingID,
			FilePath:      m.DestPath,
			FileSizeBytes: m.FileSize,
			Container:     m.ProbedInfo.Container,
			Quality:       m.ProbedInfo.Quality(),
			VideoCodec:    m.ProbedInfo.VideoCodec,
			AudioCodec:    audioCodecFromMediaInfo(m.ProbedInfo),
			FormatLabel:   m.FormatLabel,
			MediaInfoJSON: string(blob),
			PartIndex:     m.PartIndex,
		}
		if err := storage.UpsertVersion(ctx, j.DB, v); err != nil {
			return fmt.Errorf("upsert version for %q: %w", m.DestPath, err)
		}
	}
	return nil
}

// audioCodecFromMediaInfo picks the first audio stream's codec for
// the recording_versions.audio_codec column. Empty when the probe
// landed no audio streams (shouldn't happen for a real DVD remux —
// but the rename engine tolerates the zero value).
func audioCodecFromMediaInfo(info probe.MediaInfo) string {
	if len(info.AudioStreams) == 0 {
		return ""
	}
	return info.AudioStreams[0].Codec
}

// writeConversionNotes drops CONVERSION_NOTES.txt into the
// preserved-originals folder. The contents capture EXACTLY what
// makemkvcon was invoked with so the user (and anyone they trade
// with later) can verify the lossless round-trip claim.
func (j *RemuxDVDJob) writeConversionNotes(
	originalDir string, recordingID int64, commands []string, moved []movedRemux,
) error {
	now := time.Now
	if j.Now != nil {
		now = j.Now
	}
	path := filepath.Join(originalDir, conversionNotesFilename)

	var b strings.Builder
	b.WriteString("promptbook DVD remux — bit-for-bit lossless via makemkvcon\n")
	b.WriteString("===========================================================\n\n")
	b.WriteString("No transcoding was performed. The video, audio, and\n")
	b.WriteString("subtitle streams were copied byte-for-byte from the\n")
	b.WriteString("source DVD into Matroska (.mkv) containers using\n")
	b.WriteString("makemkvcon's --robot mode. The bit-for-bit DVD\n")
	b.WriteString("originals are preserved verbatim in this directory's\n")
	b.WriteString("VIDEO_TS/ subfolder so the recording can still be\n")
	b.WriteString("traded in its original DVD form.\n\n")
	fmt.Fprintf(&b, "Recording ID:  %d\n", recordingID)
	fmt.Fprintf(&b, "Conversion at: %s\n\n", now().UTC().Format(time.RFC3339))

	b.WriteString("Commands run:\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "  %s\n", cmd)
	}
	b.WriteString("\nOutput files (relative to the recording folder):\n")
	for _, m := range moved {
		fmt.Fprintf(&b, "  %s\n", filepath.Base(m.DestPath))
	}

	if err := os.WriteFile(path, []byte(b.String()), remuxFilePerm); err != nil {
		return fmt.Errorf("write %s: %w", conversionNotesFilename, err)
	}
	return nil
}

// refreshNFO rewrites movie.nfo to reflect the new
// recording_versions rows. Best-effort — the rest of the pipeline
// already committed so a failed NFO rewrite just logs.
func (j *RemuxDVDJob) refreshNFO(ctx context.Context, recordingID int64) {
	if j.NFORefresh == nil {
		j.Logger.Debug().
			Int64("recording_id", recordingID).
			Msg("remux-dvd: nforefresh not configured; skipping nfo rewrite")
		return
	}
	if err := j.NFORefresh.RewriteForRecording(ctx, recordingID); err != nil {
		j.Logger.Warn().
			Err(err).
			Int64("recording_id", recordingID).
			Msg("remux-dvd: nfo rewrite failed; continuing")
		return
	}
	j.Logger.Debug().
		Int64("recording_id", recordingID).
		Msg("remux-dvd: nfo rewritten")
}
