package rename_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/rename"
)

// fixtureRecording loads the Marigold recording from the encora testdata. It's
// the canonical "real shape" record used across the rename/nfo/ingest test
// suites.
func fixtureRecording(t *testing.T) encora.Recording {
	t.Helper()
	path := filepath.Join("..", "encora", "testdata", "recording_pilot.json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var r encora.Recording
	require.NoError(t, json.Unmarshal(b, &r))
	return r
}

func TestBuildPlan(t *testing.T) {
	t.Parallel()

	rec := fixtureRecording(t)

	tests := []struct {
		name          string
		folderTmpl    string
		fileTmpl      string
		source        string
		wantFolder    string
		wantFile      string
		wantExtension string
	}{
		{
			name:          "default_templates",
			folderTmpl:    "{Show} - {Tour} - {Date} [encora-{EncoraID}]",
			fileTmpl:      "{Show} - {Tour} - {Date} [{Master}]",
			source:        "/incoming/Marigold.mp4",
			wantFolder:    "Marigold Junction - Broadway - August 2010 [encora-90100222]",
			wantFile:      "Marigold Junction - Broadway - August 2010 [pro-shot]",
			wantExtension: ".mp4",
		},
		{
			name:          "year_only_year_token",
			folderTmpl:    "{Show} - {Tour} - {Year}",
			fileTmpl:      "{Show} - {Year}",
			source:        "/whatever.mkv",
			wantFolder:    "Marigold Junction - Broadway - 2010",
			wantFile:      "Marigold Junction - 2010",
			wantExtension: ".mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := rename.BuildPlan(rename.PlanInputs{
				Recording:      rec,
				Source:         tt.source,
				LibraryRoot:    "/library",
				FolderTemplate: tt.folderTmpl,
				FileTemplate:   tt.fileTmpl,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantFolder, plan.TargetFolder)
			assert.Equal(t, tt.wantFile, plan.TargetFile)
			assert.Equal(t, tt.wantExtension, plan.Extension)
		})
	}
}

func TestBuildPlanRejectsEmptyTemplates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		folderTmpl    string
		fileTmpl      string
		libraryRoot   string
		wantErrSubstr string
	}{
		{name: "no_root", folderTmpl: "x", fileTmpl: "y", libraryRoot: "", wantErrSubstr: "library root"},
		{name: "no_folder_tmpl", folderTmpl: "", fileTmpl: "y", libraryRoot: "/x", wantErrSubstr: "templates required"},
		{name: "no_file_tmpl", folderTmpl: "x", fileTmpl: "", libraryRoot: "/x", wantErrSubstr: "templates required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := rename.BuildPlan(rename.PlanInputs{
				Recording:      fixtureRecording(t),
				Source:         "/x.mp4",
				LibraryRoot:    tt.libraryRoot,
				FolderTemplate: tt.folderTmpl,
				FileTemplate:   tt.fileTmpl,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrSubstr)
		})
	}
}

func TestPlanAbsolutePaths(t *testing.T) {
	t.Parallel()
	plan := rename.Plan{
		LibraryRoot:  "/library",
		TargetFolder: "Show - Broadway - 2024-01-21 [encora-100]",
		TargetFile:   "Show - Broadway - 2024-01-21 [Master]",
		Extension:    ".mkv",
	}
	assert.Equal(t,
		"/library/Show - Broadway - 2024-01-21 [encora-100]/Show - Broadway - 2024-01-21 [Master].mkv",
		plan.AbsoluteFile(),
	)
}
