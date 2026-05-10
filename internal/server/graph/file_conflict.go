package graph

// file_conflict.go — destination-conflict detection for the import
// preview surface.
//
// The queue import modal calls previewQueueImport before showing the
// "Import" button. When the planned destination already has a file on
// disk, the modal needs to know whether importing would silently
// overwrite different content (warn) or whether the source and
// destination are byte-identical (no-op duplicate). The two cases
// surface different UX in the modal — overwrite needs explicit
// confirmation; duplicate just needs to inform the user.
//
// The hash short-circuits on size mismatch — stat'ing the destination
// is O(1), and any size difference is a definite "different content"
// signal without needing to read either file. Same-size files get a
// streaming sha256 comparison; for the largest files in this catalog
// (~10 GB) that's measured-in-seconds, fine for a one-time preview
// fetch.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	// have matching size + matching sha256. False when destExists is
	// false (no conflict) or when content differs.
	isDuplicate bool
}

// inspectDestinationConflict stats dest, then (when both files exist
// and sizes match) streams a sha256 comparison. Errors other than
// fs.ErrNotExist on the destination side surface as the wrapped
// error so the caller can decide whether to fail the preview or
// fall through.
//
// Cancellable: ctx is checked between hash chunks so a long
// comparison aborts cleanly when the request goes away.
func inspectDestinationConflict(ctx context.Context, src, dest string) (fileConflict, error) {
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
	if destInfo.Size() != srcInfo.Size() {
		return fileConflict{destExists: true, isDuplicate: false}, nil
	}
	same, err := filesIdentical(ctx, src, dest)
	if err != nil {
		// Hash failures (permissions, transient I/O, ...) collapse to
		// "destination exists, content unknown" — the modal still warns
		// the user, just without the "duplicate" affordance.
		return fileConflict{destExists: true, isDuplicate: false},
			fmt.Errorf("compare hashes: %w", err)
	}
	return fileConflict{destExists: true, isDuplicate: same}, nil
}

// filesIdentical streams sha256 over both paths and returns true when
// the digests match. Caller has already verified the file sizes
// match; this short-circuits on cancellation between reads.
func filesIdentical(ctx context.Context, a, b string) (bool, error) {
	hashA, err := hashFile(ctx, a)
	if err != nil {
		return false, fmt.Errorf("hash %q: %w", a, err)
	}
	hashB, err := hashFile(ctx, b)
	if err != nil {
		return false, fmt.Errorf("hash %q: %w", b, err)
	}
	if len(hashA) != len(hashB) {
		return false, nil
	}
	for i := range hashA {
		if hashA[i] != hashB[i] {
			return false, nil
		}
	}
	return true, nil
}

// hashChunkBytes is the io.Copy chunk size used by hashFile. 1 MiB
// keeps the kernel-side prefetch happy on rotational disk while
// staying small enough that ctx cancellation propagates within ~ms.
const hashChunkBytes = 1 << 20

// hashFile streams sha256 over the file at path. Cancellable via ctx
// — the chunked read loop checks ctx.Err() between reads.
func hashFile(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, hashChunkBytes)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return h.Sum(nil), nil
}
