package rename

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nicolerenee/promptbook/internal/encora"
)

// Plan describes the canonical destination for a single recording. It
// captures both the source (where the file currently lives) and the
// target (where it should land), so a dry-run can show the full diff
// without ever touching the disk.
type Plan struct {
	Source       string // current file path
	TargetFolder string // canonical folder, relative to library root
	TargetFile   string // canonical file name (no extension)
	Extension    string // ".mp4" / ".mkv" / ...
	LibraryRoot  string
}

// AbsoluteFolder returns the full destination folder for the recording,
// rooted at the library.
func (p Plan) AbsoluteFolder() string {
	return filepath.Join(p.LibraryRoot, p.TargetFolder)
}

// AbsoluteFile returns the full destination video path including extension.
func (p Plan) AbsoluteFile() string {
	return filepath.Join(p.AbsoluteFolder(), p.TargetFile+p.Extension)
}

// PlanInputs bundles the rendering inputs for BuildPlan. Keeps the
// signature small as the call site grows.
type PlanInputs struct {
	Recording      encora.Recording
	Source         string
	LibraryRoot    string
	FolderTemplate string
	FileTemplate   string
}

// BuildPlan renders the canonical names from the templates and returns a
// Plan that the mover can apply. Returns an error if either template
// produces an empty name (defensive — would otherwise silently rename
// to "/").
func BuildPlan(in PlanInputs) (*Plan, error) {
	if in.LibraryRoot == "" {
		return nil, errors.New("rename: library root is empty")
	}
	if in.FolderTemplate == "" || in.FileTemplate == "" {
		return nil, errors.New("rename: folder/file templates required")
	}

	folder, err := Apply(in.FolderTemplate, in.Recording)
	if err != nil {
		return nil, fmt.Errorf("apply folder template: %w", err)
	}
	if folder == "" {
		return nil, errors.New("rename: folder template rendered empty")
	}

	file, err := Apply(in.FileTemplate, in.Recording)
	if err != nil {
		return nil, fmt.Errorf("apply file template: %w", err)
	}
	if file == "" {
		return nil, errors.New("rename: file template rendered empty")
	}

	ext := strings.ToLower(filepath.Ext(in.Source))
	return &Plan{
		Source:       in.Source,
		TargetFolder: folder,
		TargetFile:   file,
		Extension:    ext,
		LibraryRoot:  in.LibraryRoot,
	}, nil
}
