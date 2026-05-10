package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/config"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestAPISettingsRedactsKeys is the load-bearing test for the settings
// endpoint: the response must surface api_key_set as a boolean rather
// than echoing the raw key. This guards against an accidental refactor
// that ships the api keys to anyone who can hit /api/v1/settings.
func TestAPISettingsRedactsKeys(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	cfg := config.Config{
		Encora: config.EncoraConfig{
			BaseURL:   "https://encora.it",
			APIKey:    "super-secret-encora-key",
			UserAgent: "promptbook/test",
			RateLimit: config.RateLimitConfig{RequestsPerMinute: 30, BurstReserve: 2},
		},
		Storage: config.StorageConfig{DatabasePath: "/tmp/promptbook.db"},
		Library: config.LibraryConfig{
			Root:           "/store/library",
			FolderTemplate: "{Show} - {Tour} - {Date} [encora-{EncoraID}]",
			FileTemplate:   "{Show} - {Tour} - {Date} [{Master}]",
			IncomingDirs:   []string{"/store/incoming"},
			WatchInterval:  time.Minute,
		},
		Server: config.ServerConfig{
			Listen: "[::]:8080",
			OIDC: config.OIDCConfig{
				Issuer:      "https://freckle.id",
				Audience:    "promptbook",
				JWKSRefresh: time.Hour,
			},
		},
		Stagemedia: config.StagemediaConfig{
			BaseURL:   "https://stagemedia.me",
			APIKey:    "", // explicitly unset to assert the false branch
			UserAgent: "promptbook/test",
		},
	}

	srv, err := server.New(server.Options{
		DB:           db,
		Config:       cfg,
		Version:      "v1.2.3",
		ConfigSource: "/etc/promptbook.yaml",
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/settings", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// The body should never contain the raw key text.
	assert.NotContains(t, rr.Body.String(), "super-secret-encora-key")

	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))

	enc, ok := got["encora"].(map[string]any)
	require.True(t, ok, "encora object missing")
	apiKeySet, ok := enc["api_key_set"].(bool)
	require.True(t, ok, "encora.api_key_set should be a bool")
	assert.True(t, apiKeySet, "encora.api_key_set should be true when key is configured")
	_, hasAPIKey := enc["api_key"]
	assert.False(t, hasAPIKey, "encora.api_key must not appear in the response")
	assert.Equal(t, "https://encora.it", enc["base_url"])

	rateLimit, ok := enc["rate_limit"].(map[string]any)
	require.True(t, ok)
	assert.InEpsilon(t, float64(30), rateLimit["requests_per_minute"], 0.0001)
	assert.InEpsilon(t, float64(2), rateLimit["burst_reserve"], 0.0001)

	sm, ok := got["stagemedia"].(map[string]any)
	require.True(t, ok)
	smKeySet, ok := sm["api_key_set"].(bool)
	require.True(t, ok)
	assert.False(t, smKeySet, "stagemedia.api_key_set should be false when key is empty")

	library, ok := got["library"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1m0s", library["watch_interval"])
	dirs, ok := library["incoming_dirs"].([]any)
	require.True(t, ok, "library.incoming_dirs should be an array")
	assert.Len(t, dirs, 1)

	srvBlock, ok := got["server"].(map[string]any)
	require.True(t, ok)
	oidc, ok := srvBlock["oidc"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1h0m0s", oidc["jwks_refresh"])

	assert.Equal(t, "v1.2.3", got["version"])
	assert.Equal(t, "/etc/promptbook.yaml", got["config_source"])
}

// TestAPISettingsEmptyConfig verifies that with a zero-value Config the
// response renders empty strings + an empty (not-null) incoming_dirs
// list, both api_key_set fields false, and the configSource sentinel.
func TestAPISettingsEmptyConfig(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/settings", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))

	enc := got["encora"].(map[string]any)
	assert.Equal(t, false, enc["api_key_set"])

	library := got["library"].(map[string]any)
	dirs, ok := library["incoming_dirs"].([]any)
	require.True(t, ok, "incoming_dirs must be [] not null")
	assert.Empty(t, dirs)
	assert.Equal(t, "0s", library["watch_interval"])

	assert.Equal(t, "dev", got["version"])
	assert.Equal(t, "<not exposed>", got["config_source"])
}

// (TestSettingsPageRenders was retired with the SPA migration. The
// /settings route now resolves to the same Mithril shell as every
// other browser-facing path; coverage moved to TestSPAShell. The
// /api/v1/settings JSON contract — which the SPA actually consumes —
// is still exercised by TestSettingsAPI above.)
