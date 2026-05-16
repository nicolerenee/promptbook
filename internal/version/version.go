// Package version is the single source of truth for build-stamped
// identity (version, commit, build date, builder) and the canonical
// User-Agent string every outbound HTTP client sends.
//
// The build pipeline overrides these via -ldflags
// (-X github.com/nicolerenee/promptbook/internal/version.Version=…).
// The cmd package re-exports them so `promptbook --version` keeps the
// same surface; HTTP clients (Encora, StageMedia, TMDB, future
// providers) consume UserAgent() so the wire identity is uniform and
// not user-tweakable.
package version

// Build-time variables overridden via -ldflags.
//
//nolint:gochecknoglobals // build-time variables set via ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
	BuiltBy   = "unknown"
)

// UserAgent returns the canonical User-Agent string every outbound
// HTTP client should set. Format is "promptbook/{version}". Hardcoded
// at build time and intentionally NOT configurable — letting users
// override the UA breaks rate-limit attribution upstream and confuses
// API providers tracking which client is misbehaving.
func UserAgent() string {
	return "promptbook/" + Version
}
