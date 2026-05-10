// Package config provides application configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Defaults.
const (
	DefaultEncoraBaseURL       = "https://encora.it"
	DefaultRequestsPerMinute   = 30
	DefaultBurstReserve        = 2
	DefaultDatabasePath        = "./promptbook.db"
	DefaultListenAddr          = "[::]:8080"
	DefaultJWKSRefreshInterval = 1 * time.Hour
	// DefaultFolderTemplate uses the new {DateWithVariant} ISO-partial
	// date token so partial-month / variant-disambiguated recordings
	// render cleanly in the folder name without dangling brackets.
	DefaultFolderTemplate = "{Show} ({DateWithVariant}) [encora-{EncoraID}]"
	// DefaultFileTemplate exercises the optional-segment grammar so
	// empty Tour / Master / probe results / part index collapse to no
	// output rather than leaving "[]" or " - " stubs in the filename.
	DefaultFileTemplate = "{Show} ({DateWithVariant}) [encora-{EncoraID}]" +
		"{? - {Tour}}{?[{Master}]}{?[{VideoCodec}]}{?[{Quality}]}" +
		"{? - part-{Part}}"
	// DefaultFFProbePath points at `ffprobe` on PATH. Override via
	// library.ffprobePath / PROMPTBOOK_LIBRARY_FFPROBEPATH when the
	// binary lives elsewhere.
	DefaultFFProbePath = "ffprobe"
	// DefaultFFmpegPath points at `ffmpeg` on PATH. ffmpeg is used by
	// the picker's fanart-fallback path to extract still frames from
	// the local video file when Encora has no curated screenshots for
	// a recording. Override via library.ffmpegPath /
	// PROMPTBOOK_LIBRARY_FFMPEGPATH when the binary lives elsewhere
	// (e.g. a vendored static build); the extractor surfaces a clear
	// "not available" error if ffmpeg isn't reachable at extraction
	// time.
	DefaultFFmpegPath        = "ffmpeg"
	DefaultWatchInterval     = 1 * time.Minute
	DefaultStagemediaBaseURL = "https://stagemedia.me"
	// DefaultTMDBBaseURL is TMDB's v3 API root. Override via
	// tmdb.baseUrl / PROMPTBOOK_TMDB_BASEURL only for offline testing
	// (e.g. an httptest server). Production should always speak to
	// the canonical API root.
	DefaultTMDBBaseURL = "https://api.themoviedb.org/3"
)

// Config is the top-level application configuration.
type Config struct {
	Encora     EncoraConfig     `mapstructure:"encora"`
	Storage    StorageConfig    `mapstructure:"storage"`
	Library    LibraryConfig    `mapstructure:"library"`
	Server     ServerConfig     `mapstructure:"server"`
	Stagemedia StagemediaConfig `mapstructure:"stagemedia"`
	TMDB       TMDBConfig       `mapstructure:"tmdb"`
}

// EncoraConfig holds Encora API client configuration.
type EncoraConfig struct {
	BaseURL   string          `mapstructure:"baseUrl"`
	APIKey    string          `mapstructure:"apiKey"`
	RateLimit RateLimitConfig `mapstructure:"rateLimit"`
}

// RateLimitConfig caps how aggressively we hit Encora.
type RateLimitConfig struct {
	RequestsPerMinute int `mapstructure:"requestsPerMinute"`
	BurstReserve      int `mapstructure:"burstReserve"`
}

// StorageConfig holds local cache storage paths.
type StorageConfig struct {
	DatabasePath string `mapstructure:"databasePath"`
}

// LibraryConfig describes the on-disk media library and naming scheme.
//
// ImageRoot is the on-disk directory for cached posters/backdrops/headshots.
// Empty (the default) disables the image cache entirely — no downloads
// during sync, no /images/* serving from the HTTP server. Set explicitly
// to opt in; promptbook will create subdirectories under it as needed.
type LibraryConfig struct {
	Root           string        `mapstructure:"root"`
	FolderTemplate string        `mapstructure:"folderTemplate"`
	FileTemplate   string        `mapstructure:"fileTemplate"`
	IncomingDirs   []string      `mapstructure:"incomingDirs"`
	WatchInterval  time.Duration `mapstructure:"watchInterval"`
	ImageRoot      string        `mapstructure:"imageRoot"`
	// FFProbePath is the ffprobe binary used by the rename engine to
	// extract codec/resolution metadata for the new {Container} /
	// {VideoCodec} / {Quality} tokens. Empty falls back to "ffprobe"
	// on PATH; the engine surfaces an error if the binary isn't
	// reachable at probe time (no silent empty-mediainfo fallback).
	FFProbePath string `mapstructure:"ffprobePath"`
	// FFmpegPath is the ffmpeg binary used by the picker's fanart-
	// fallback to extract still frames from the local video file.
	// Empty falls back to "ffmpeg" on PATH; the picker surfaces an
	// empty options array (with a logged reason) when ffmpeg is
	// unavailable, so the rest of the modal stays usable.
	FFmpegPath string `mapstructure:"ffmpegPath"`
}

// ServerConfig holds HTTP server configuration (used by `promptbook serve`).
//
// PublicURL is the externally-reachable base URL of the promptbook server.
// Used as the base for image URLs written into NFO files so media servers
// can fetch posters, fanart, and actor headshots over HTTP. Empty disables
// URL emission — NFOs fall back to local sibling files (poster.jpg,
// fanart.jpg) for movie images and skip actor thumbs entirely. No default
// is set: the empty string is a valid (no-URL) state that mirrors how
// local-only setups expect the writer to behave.
type ServerConfig struct {
	Listen    string     `mapstructure:"listen"`
	PublicURL string     `mapstructure:"publicURL"`
	OIDC      OIDCConfig `mapstructure:"oidc"`
}

// OIDCConfig configures JWT validation for /api/v1/*.
type OIDCConfig struct {
	Issuer      string        `mapstructure:"issuer"`
	Audience    string        `mapstructure:"audience"`
	JWKSRefresh time.Duration `mapstructure:"jwksRefresh"`
}

// StagemediaConfig holds StageMedia.me API client configuration. Optional —
// leave APIKey blank to disable poster + headshot fetching.
type StagemediaConfig struct {
	BaseURL string `mapstructure:"baseUrl"`
	APIKey  string `mapstructure:"apiKey"`
}

// TMDBConfig holds TMDB API client configuration. Optional — leave
// APIKey blank to disable the TMDB picker source (poster + fanart
// suggestions for recordings that carry a TMDB / IMDB external id).
// The picker handlers nil-check and degrade to "no TMDB options"
// cleanly when the key is unset.
type TMDBConfig struct {
	BaseURL string `mapstructure:"baseUrl"`
	APIKey  string `mapstructure:"apiKey"`
}

// LoadOptions configures how configuration is loaded.
type LoadOptions struct {
	// ConfigFile is an explicit config file path. If empty, default locations are searched.
	ConfigFile string
}

// Load reads configuration from file and environment variables.
//
// Search order (when ConfigFile is empty):
//
//   - $HOME/promptbook.yaml or $HOME/.promptbook.yaml or $HOME/config.yaml
//   - ./promptbook.yaml or ./.promptbook.yaml or ./config.yaml
//   - /config/promptbook.yaml or /config/.promptbook.yaml or /config/config.yaml
//
// Env vars with prefix PROMPTBOOK_ override config file values.
// E.g. PROMPTBOOK_ENCORA_APIKEY overrides encora.apiKey.
func Load(opts LoadOptions) (Config, error) {
	v := viper.NewWithOptions(viper.ExperimentalBindStruct())
	setDefaults(v)

	if opts.ConfigFile != "" {
		v.SetConfigFile(opts.ConfigFile)
	} else {
		home, err := os.UserHomeDir()
		if err == nil {
			v.AddConfigPath(home)
		}
		v.AddConfigPath(".")
		v.AddConfigPath("/config")
		v.SetConfigType("yaml")
		v.SetConfigName("promptbook")
		v.SetConfigName(".promptbook")
		v.SetConfigName("config")
	}

	v.SetEnvPrefix("PROMPTBOOK")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !asConfigNotFound(err, &notFound) {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("encora.baseUrl", DefaultEncoraBaseURL)
	v.SetDefault("encora.rateLimit.requestsPerMinute", DefaultRequestsPerMinute)
	v.SetDefault("encora.rateLimit.burstReserve", DefaultBurstReserve)
	v.SetDefault("storage.databasePath", DefaultDatabasePath)
	v.SetDefault("library.folderTemplate", DefaultFolderTemplate)
	v.SetDefault("library.fileTemplate", DefaultFileTemplate)
	v.SetDefault("library.watchInterval", DefaultWatchInterval)
	v.SetDefault("library.ffprobePath", DefaultFFProbePath)
	v.SetDefault("library.ffmpegPath", DefaultFFmpegPath)
	v.SetDefault("server.listen", DefaultListenAddr)
	v.SetDefault("server.oidc.jwksRefresh", DefaultJWKSRefreshInterval)
	v.SetDefault("stagemedia.baseUrl", DefaultStagemediaBaseURL)
	v.SetDefault("tmdb.baseUrl", DefaultTMDBBaseURL)
}

// asConfigNotFound reports whether err is viper.ConfigFileNotFoundError.
// Implemented as a small helper so callers don't need to import viper.
func asConfigNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if e, ok := err.(viper.ConfigFileNotFoundError); ok { //nolint:errorlint // viper sentinel is value, not wrapped
		*target = e
		return true
	}
	return false
}
