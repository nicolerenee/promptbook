package server

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// settingsResponse is the JSON shape /api/v1/settings returns. It
// mirrors config.Config but with secret material redacted to a boolean
// (api_key_set) and time.Duration fields rendered as their .String()
// form (e.g. "1m0s") so the read-only settings page can present them
// verbatim.
type settingsResponse struct {
	Encora       settingsEncora     `json:"encora"`
	Storage      settingsStorage    `json:"storage"`
	Library      settingsLibrary    `json:"library"`
	Server       settingsServer     `json:"server"`
	Stagemedia   settingsStagemedia `json:"stagemedia"`
	Version      string             `json:"version"`
	ConfigSource string             `json:"config_source"`
}

type settingsEncora struct {
	BaseURL   string            `json:"base_url"`
	APIKeySet bool              `json:"api_key_set"`
	UserAgent string            `json:"user_agent"`
	RateLimit settingsRateLimit `json:"rate_limit"`
}

type settingsRateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute"`
	BurstReserve      int `json:"burst_reserve"`
}

type settingsStorage struct {
	DatabasePath string `json:"database_path"`
}

type settingsLibrary struct {
	Root           string   `json:"root"`
	FolderTemplate string   `json:"folder_template"`
	FileTemplate   string   `json:"file_template"`
	IncomingDirs   []string `json:"incoming_dirs"`
	WatchInterval  string   `json:"watch_interval"`
}

type settingsServer struct {
	Listen string       `json:"listen"`
	OIDC   settingsOIDC `json:"oidc"`
}

type settingsOIDC struct {
	Issuer      string `json:"issuer"`
	Audience    string `json:"audience"`
	JWKSRefresh string `json:"jwks_refresh"`
}

type settingsStagemedia struct {
	BaseURL   string `json:"base_url"`
	APIKeySet bool   `json:"api_key_set"`
	UserAgent string `json:"user_agent"`
}

// handleSettings returns the loaded application configuration with
// secret fields (api keys) collapsed to a boolean. The endpoint is
// read-only — the /settings page is documentation, not a control panel,
// so there is no companion POST.
func (s *Server) handleSettings(c echo.Context) error {
	cfg := s.config

	// IncomingDirs may be nil when no library.incomingDirs is
	// configured; coerce to an empty slice so the JSON renders [] rather
	// than null and the JS template doesn't have to nil-check.
	incoming := cfg.Library.IncomingDirs
	if incoming == nil {
		incoming = []string{}
	}

	resp := settingsResponse{
		Encora: settingsEncora{
			BaseURL:   cfg.Encora.BaseURL,
			APIKeySet: cfg.Encora.APIKey != "",
			UserAgent: cfg.Encora.UserAgent,
			RateLimit: settingsRateLimit{
				RequestsPerMinute: cfg.Encora.RateLimit.RequestsPerMinute,
				BurstReserve:      cfg.Encora.RateLimit.BurstReserve,
			},
		},
		Storage: settingsStorage{DatabasePath: cfg.Storage.DatabasePath},
		Library: settingsLibrary{
			Root:           cfg.Library.Root,
			FolderTemplate: cfg.Library.FolderTemplate,
			FileTemplate:   cfg.Library.FileTemplate,
			IncomingDirs:   incoming,
			WatchInterval:  cfg.Library.WatchInterval.String(),
		},
		Server: settingsServer{
			Listen: cfg.Server.Listen,
			OIDC: settingsOIDC{
				Issuer:      cfg.Server.OIDC.Issuer,
				Audience:    cfg.Server.OIDC.Audience,
				JWKSRefresh: cfg.Server.OIDC.JWKSRefresh.String(),
			},
		},
		Stagemedia: settingsStagemedia{
			BaseURL:   cfg.Stagemedia.BaseURL,
			APIKeySet: cfg.Stagemedia.APIKey != "",
			UserAgent: cfg.Stagemedia.UserAgent,
		},
		Version:      s.version,
		ConfigSource: s.configSource,
	}
	return c.JSON(http.StatusOK, resp)
}

// handleSettingsPage renders the read-only settings shell. The body
// fetches /api/v1/settings and renders into #page-root.
func (s *Server) handleSettingsPage(c echo.Context) error {
	return c.Render(http.StatusOK, "settings.html", shellData{
		Title:     "Settings",
		ActiveNav: "settings",
		Version:   s.version,
	})
}
