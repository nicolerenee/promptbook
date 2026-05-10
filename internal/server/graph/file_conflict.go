package graph

// file_conflict.go — destination-conflict detection for the import
// preview surface.
//
// The queue import modal calls previewQueueImport before showing the
// "Import" button. When the planned destination already has a file on
// disk, the modal needs to know whether importing would silently
// overwrite different content (warn) or whether the source and
// destination are the same file (no-op duplicate). The two cases
// surface different UX in the modal — overwrite needs explicit
// confirmation; duplicate just needs to inform the user.
//
// Duplicate detection is byte-size-only. We previously stream-hashed
// sha256 over both files when sizes matched, but for the typical
// ~10 GB recording that took 60-90s per preview — long enough that
// the import modal felt velvet-antlers. Two video files with identical byte
// counts but different content is vanishingly rare in practice
// (codec + container + duration + audio tracks would all have to
// land on the same total size by accident), and the cost of a false
// "duplicate" surface is low — the user notices, picks Skip in the
// modal, moves on. The cost of a 90-second freeze on every preview
// is much higher.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// fileConflict captures the result of comparing a candidate source
// file against a planned destination on disk.
type fileConflict struct {
	// destExists is true when stat(dest) succeeded — the destination
	// path is occupied.
	destExists bool
	// isDuplicate is true when destExists AND the source + destination
	// have matching size. False when destExists is false (no conflict)
	// or when sizes differ.
	isDuplicate bool
}

// inspectDestinationConflict stats both paths and reports the
// conflict shape. Pure stat-only — no file reads, no hashing — so
// the call is microseconds regardless of file size. The ctx
// parameter stays in the signature so future callers can pass a
// cancellable context if a slow filesystem stat ever needs aborting,
// but the current implementation doesn't read it.
func inspectDestinationConflict(_ context.Context, src, dest string) (fileConflict, error) {
	destInfo, err := os.Stat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return fileConflict{}, nil
	}
	if err != nil {
		return fileConflict{}, fmt.Errorf("stat dest %q: %w", dest, err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		// A source we can't stat isn't a duplicate decision — let the
		// downstream importer surface the error.
		return fileConflict{destExists: true}, nil //nolint:nilerr // intentional fall-through.
	}
	return fileConflict{
		destExists:  true,
		isDuplicate: destInfo.Size() == srcInfo.Size(),
	}, nil
}
