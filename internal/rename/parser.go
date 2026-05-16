// Package rename owns the canonical naming of recordings on disk:
// extracting the encora id from a path, computing the target folder/file
// names from configurable templates, and applying the rename safely.
package rename

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoEncoraID indicates the resolver couldn't find an id in any of the
// recognized locations. Callers can use this to decide whether to skip,
// prompt, or surface to the user.
var ErrNoEncoraID = errors.New("rename: no encora id found")

// idPattern matches all four documented forms (case-insensitive):
//
//	[encora-12345]   {e-12345}   [e-12345]   (encora-12345)
//
// The pattern intentionally does not anchor to either side so it matches
// inside a longer file or folder name.
var idPattern = regexp.MustCompile(`(?i)[\[\{\(](?:encora|e)-(\d+)[\]\}\)]`)

// SidecarFilename is the recognized sidecar name. Lower-case to match
// userland convention; check is exact-match (no case fallback).
const SidecarFilename = ".encora-id"

// ResolveSource describes where an encora id came from for logging /
// dry-run output.
type ResolveSource string

// ResolveSource constants — listed in precedence order. Resolve walks
// these in turn and returns on the first match.
const (
	SourceFlag     ResolveSource = "flag"
	SourceSidecar  ResolveSource = "sidecar"
	SourceFilename ResolveSource = "filename"
	SourceFolder   ResolveSource = "folder"
)

// Resolve walks the precedence chain — explicit > sidecar > filename >
// folder name — and returns the first hit. path may be a file or a
// directory; the caller decides which sidecar/folder is "the" container.
//
// Pass flagID = 0 if no explicit id was given on the CLI.
func Resolve(path string, flagID int) (int64, ResolveSource, error) {
	if flagID > 0 {
		return int64(flagID), SourceFlag, nil
	}

	containerDir := containerDirOf(path)
	if id, ok := readSidecar(containerDir); ok {
		return id, SourceSidecar, nil
	}

	if id, ok := matchID(filepath.Base(path)); ok {
		return id, SourceFilename, nil
	}
	// Try the parent folder name if path was a file.
	if path != containerDir {
		if id, ok := matchID(filepath.Base(containerDir)); ok {
			return id, SourceFolder, nil
		}
	}
	return 0, "", ErrNoEncoraID
}

// containerDirOf returns the directory that "owns" the given path —
// the path itself if it's a directory, otherwise its parent.
func containerDirOf(path string) string {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return path
	}
	return filepath.Dir(path)
}

func readSidecar(dir string) (int64, bool) {
	full := filepath.Join(dir, SidecarFilename)
	b, err := os.ReadFile(full)
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// idPatternSubmatchSize is the expected len() of FindStringSubmatch when
// the regex matches: full match + one capture group.
const idPatternSubmatchSize = 2

func matchID(name string) (int64, bool) {
	matches := idPattern.FindStringSubmatch(name)
	if len(matches) != idPatternSubmatchSize {
		return 0, false
	}
	id, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// FormatResolveError wraps ErrNoEncoraID with the path that failed.
func FormatResolveError(path string) error {
	return fmt.Errorf("%w at %s", ErrNoEncoraID, path)
}
