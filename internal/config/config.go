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
	DefaultEncoraUserAgent     = "promptbook/0.0.1"
	DefaultRequestsPerMinute   = 30
	DefaultBurstReserve        = 2
	DefaultDatabasePath        = "./promptbook.db"
	DefaultListenAddr          = "[::]:8080"
	DefaultJWKSRefreshInterval = 1 * time.Hour
	DefaultFolderTemplate      = "{Show} - {Tour} - {Date} [encora-{EncoraID}]"
	DefaultFileTemplate        = "{Show} - {Tour} - {Date} [{Master}]"
	DefaultWatchInterval       = 1 * time.Minute
)

// Config is the top-level application configuration.
type Config struct {
	Encora  EncoraConfig  `mapstructure:"encora"`
	Storage StorageConfig `mapstructure:"storage"`
	Library LibraryConfig `mapstructure:"library"`
	Server  ServerConfig  `mapstructure:"server"`
}

// EncoraConfig holds Encora API client configuration.
type EncoraConfig struct {
	BaseURL   string          `mapstructure:"baseUrl"`
	APIKey    string          `mapstructure:"apiKey"`
	UserAgent string          `mapstructure:"userAgent"`
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
type LibraryConfig struct {
	Root           string        `mapstructure:"root"`
	FolderTemplate string        `mapstructure:"folderTemplate"`
	FileTemplate   string        `mapstructure:"fileTemplate"`
	IncomingDirs   []string      `mapstructure:"incomingDirs"`
	WatchInterval  time.Duration `mapstructure:"watchInterval"`
}

// ServerConfig holds HTTP server configuration (used by `promptbook serve`).
type ServerConfig struct {
	Listen string     `mapstructure:"listen"`
	OIDC   OIDCConfig `mapstructure:"oidc"`
}

// OIDCConfig configures JWT validation for /api/v1/*.
type OIDCConfig struct {
	Issuer      string        `mapstructure:"issuer"`
	Audience    string        `mapstructure:"audience"`
	JWKSRefresh time.Duration `mapstructure:"jwksRefresh"`
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
	v.SetDefault("encora.userAgent", DefaultEncoraUserAgent)
	v.SetDefault("encora.rateLimit.requestsPerMinute", DefaultRequestsPerMinute)
	v.SetDefault("encora.rateLimit.burstReserve", DefaultBurstReserve)
	v.SetDefault("storage.databasePath", DefaultDatabasePath)
	v.SetDefault("library.folderTemplate", DefaultFolderTemplate)
	v.SetDefault("library.fileTemplate", DefaultFileTemplate)
	v.SetDefault("library.watchInterval", DefaultWatchInterval)
	v.SetDefault("server.listen", DefaultListenAddr)
	v.SetDefault("server.oidc.jwksRefresh", DefaultJWKSRefreshInterval)
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
