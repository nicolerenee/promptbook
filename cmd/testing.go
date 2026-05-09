package cmd

import (
	"context"
	"io"

	"github.com/nicolerenee/promptbook/internal/config"
)

// RunForTest is a test helper that runs the root cobra command with the
// given context, args, and a writer for stdout/stderr. Not safe for
// parallel use — root cmd state is global.
func RunForTest(ctx context.Context, args []string, out io.Writer) error {
	resetGlobalsForTest()
	rootCmd.SetContext(ctx)
	rootCmd.SetArgs(args)
	if out != nil {
		rootCmd.SetOut(out)
		rootCmd.SetErr(out)
	}
	return rootCmd.Execute()
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
