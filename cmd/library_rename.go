package cmd

import (
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	libraryRenameEncoraID int
	libraryRenameDryRun   bool
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryRenameCmd = &cobra.Command{
	Use:   "rename PATH",
	Short: "Rename a single recording in place to the canonical scheme",
	Long: `Renames a video file or folder-with-video to ` +
		"`library.folderTemplate`/`library.fileTemplate`" + `. No subtitle
download, no NFO. Useful for fixing a folder that's already in the library
under the wrong scheme. Use ` + "`library ingest`" + ` for the full workflow.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryRename,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryRenameCmd.Flags().IntVar(
		&libraryRenameEncoraID,
		"encora-id",
		0,
		"explicit Encora recording ID (overrides sidecar/filename detection)",
	)
	libraryRenameCmd.Flags().BoolVar(
		&libraryRenameDryRun,
		"dry-run",
		false,
		"show what would change without touching the filesystem",
	)
	libraryCmd.AddCommand(libraryRenameCmd)
}

func runLibraryRename(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	src := args[0]

	if appConfig.Library.Root == "" {
		return errors.New("library.root is not configured")
	}

	id, source, err := rename.Resolve(src, libraryRenameEncoraID)
	if err != nil {
		return fmt.Errorf("resolve encora id: %w", err)
	}

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	loaded, err := storage.LoadRecording(ctx, db, id)
	if err != nil {
		return fmt.Errorf("load recording: %w", err)
	}

	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      loaded.Recording,
		Source:         src,
		LibraryRoot:    appConfig.Library.Root,
		FolderTemplate: appConfig.Library.FolderTemplate,
		FileTemplate:   appConfig.Library.FileTemplate,
	})
	if err != nil {
		return fmt.Errorf("build plan: %w", err)
	}

	if libraryRenameDryRun {
		log.Info().
			Str("source", src).
			Int64("encora_id", id).
			Str("resolved_from", string(source)).
			Str("dest", plan.AbsoluteFile()).
			Msg("would rename")
		return nil
	}

	dest, err := plan.Apply()
	if err != nil {
		return fmt.Errorf("apply plan: %w", err)
	}
	if sErr := plan.EnsureSidecar(id); sErr != nil {
		log.Warn().Err(sErr).Msg("failed to write sidecar")
	}
	log.Info().
		Str("source", src).
		Int64("encora_id", id).
		Str("dest", dest).
		Msg("renamed")
	return nil
}
