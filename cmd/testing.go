package cmd

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/config"
)

// RunForTest is a test helper that runs the root cobra command with the
// given context, args, and a writer for stdout/stderr. Not safe for
// parallel use — root cmd state is global.
func RunForTest(ctx context.Context, args []string, out io.Writer) error {
	resetGlobalsForTest()
	// Cobra only propagates the parent's ctx to a subcommand when the
	// subcommand's ctx is nil (see cobra's command.go ExecuteC). Across
	// successive test invocations the previous test's canceled context
	// would otherwise stick to each subcommand. Clear them all so the new
	// ctx propagates cleanly.
	clearSubcommandContexts(rootCmd)
	rootCmd.SetContext(ctx)
	rootCmd.SetArgs(args)
	if out != nil {
		rootCmd.SetOut(out)
		rootCmd.SetErr(out)
	}
	return rootCmd.Execute()
}

// clearSubcommandContexts walks the cobra tree under root and resets
// each subcommand's context to nil. See RunForTest for why this is
// necessary between test invocations.
func clearSubcommandContexts(root *cobra.Command) {
	for _, sub := range root.Commands() {
		sub.SetContext(nil) //nolint:staticcheck // intentionally nil to re-enable cobra's parent-ctx fallback
		clearSubcommandContexts(sub)
	}
}

// resetGlobalsForTest zeroes flag/config globals so a previous run
// doesn't leak state into the next. Reset ingest flag globals too so
// `library ingest` invocations between tests pick up fresh defaults.
func resetGlobalsForTest() {
	cfgFile = ""
	logLevel = "info"
	logPretty = false
	showVersion = false
	appConfig = config.Config{}

	ingestEncoraID = 0
	ingestDryRun = false
	ingestInteractive = false
	ingestAddToCollection = false
	libraryRenameEncoraID = 0
	libraryRenameDryRun = false
	libraryNFODryRun = false
}
