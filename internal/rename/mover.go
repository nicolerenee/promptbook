package rename

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrTargetExists is returned when the destination already exists. The
// mover never overwrites — callers must resolve the conflict (move the
// existing file aside, or skip).
var ErrTargetExists = errors.New("rename: target file already exists")

// libraryDirPerm is the permission used when creating canonical folders.
// 0o755 is intentional — the user mounts the library into a Jellyfin
// container that runs as a different uid, so other-readable is required.
const libraryDirPerm = 0o755

// libraryFilePerm matches libraryDirPerm's intent: world-readable for
// downstream consumers (Jellyfin, the user's filesystem browser).
const libraryFilePerm = 0o644

// Apply moves Source to AbsoluteFile, creating the target folder along
// the way. Uses os.Rename when source/target are on the same device and
// falls back to copy+remove for cross-device moves.
//
// Returns the absolute target file path on success.
//
// When Source and AbsoluteFile resolve to the same on-disk entry (the
// file is already at its canonical location — common when a library
// scan picks up files that already follow the configured naming
// convention) Apply short-circuits to a no-op success and returns
// dest. Without this branch the existing-target guard would refuse,
// since stat'ing the destination always succeeds.
func (p Plan) Apply() (string, error) {
	dest := p.AbsoluteFile()
	destInfo, destErr := os.Stat(dest)
	if destErr == nil {
		srcInfo, srcErr := os.Stat(p.Source)
		if srcErr == nil && os.SameFile(srcInfo, destInfo) {
			return dest, nil
		}
		return "", fmt.Errorf("%w: %s", ErrTargetExists, dest)
	} else if !errors.Is(destErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat target: %w", destErr)
	}

	if err := os.MkdirAll(p.AbsoluteFolder(), libraryDirPerm); err != nil {
		return "", fmt.Errorf("mkdir target folder: %w", err)
	}

	renameErr := os.Rename(p.Source, dest)
	if renameErr == nil {
		return dest, nil
	}
	if !isCrossDevice(renameErr) {
		return "", fmt.Errorf("rename: %w", renameErr)
	}
	if copyErr := copyAndRemove(p.Source, dest); copyErr != nil {
		return "", copyErr
	}
	return dest, nil
}

func copyAndRemove(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(
		dest,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		libraryFilePerm,
	)
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

// isCrossDevice recognizes the EXDEV (cross-device link) error from
// os.Rename via the wrapped LinkError's message text. Falls back to
// substring matching for portability across Linux/macOS where the
// underlying syscall error wording differs.
func isCrossDevice(err error) bool {
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

// EnsureSidecar drops a .encora-id sidecar in the canonical folder so a
// later rename run has a fast path even without the encora-id token in
// the file name. Idempotent.
func (p Plan) EnsureSidecar(id int64) error {
	path := filepath.Join(p.AbsoluteFolder(), SidecarFilename)
	content := fmt.Sprintf("%d\n", id)
	if err := os.WriteFile(path, []byte(content), libraryFilePerm); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}
