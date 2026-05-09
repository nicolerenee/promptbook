// Package cmd provides the CLI entry point.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/config"
)

// Version information - set at build time via ldflags.
//
//nolint:gochecknoglobals // build-time variables set via ldflags
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
	BuiltBy   = "unknown"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	cfgFile   string
	logLevel  string
	logPretty bool

	showVersion bool
)

// appConfig holds the loaded configuration for use by subcommands.
//
//nolint:gochecknoglobals // shared across cobra subcommand RunE funcs.
var appConfig config.Config

// rootCmd represents the base command.
//
//nolint:gochecknoglobals // cobra requires package-level command variable
var rootCmd = &cobra.Command{
	Use:   "promptbook",
	Short: "Encora-backed catalog and library tools for Broadway recordings",
	Long: `Promptbook mirrors your Encora collection locally, renames recordings on disk
into a canonical scheme that bakes the Encora ID into the folder name, and
generates Jellyfin-compatible NFO metadata so your Performances library reads
clean without bouncing through TMDB.`,
	SilenceUsage: true,
}

// Execute runs the root command.
func Execute() {
	for _, arg := range os.Args[1:] {
		if arg == "-V" || arg == "--version" {
			printVersion()
			return
		}
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

//nolint:gochecknoinits // cobra requires init for flag registration
func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(
		&cfgFile,
		"config",
		"",
		"config file (default: search $HOME, ., /config for promptbook.yaml or config.yaml)",
	)
	rootCmd.PersistentFlags().BoolVarP(
		&showVersion,
		"version",
		"V",
		false,
		"print version information and exit",
	)
	rootCmd.PersistentFlags().StringVar(
		&logLevel,
		"log-level",
		"info",
		"log level (debug, info, warn, error)",
	)
	rootCmd.PersistentFlags().BoolVar(
		&logPretty,
		"log-pretty",
		false,
		"enable pretty (human-readable) logging",
	)
}

//nolint:forbidigo // CLI version output requires fmt.Printf
func printVersion() {
	fmt.Printf("promptbook %s\n", Version)
	fmt.Printf("  commit:   %s\n", Commit)
	fmt.Printf("  built:    %s\n", BuildDate)
	fmt.Printf("  built by: %s\n", BuiltBy)
}

func initConfig() {
	cfg, err := config.Load(config.LoadOptions{ConfigFile: cfgFile})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}
	appConfig = cfg

	setupLogging()
}

func setupLogging() {
	switch strings.ToLower(logLevel) {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	if logPretty {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}) //nolint:reassign // standard zerolog pattern
	}
}
