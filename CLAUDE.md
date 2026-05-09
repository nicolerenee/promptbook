# Claude Code Instructions

This document guides Claude Code when working in this repo.

## What promptbook is

Promptbook is an Encora-backed catalog and library-tooling CLI/server for
Broadway recordings. It mirrors a user's Encora collection into a local SQLite
cache, renames recordings on disk to a canonical scheme with the Encora ID
baked into the folder name, and generates Jellyfin-compatible NFO files.

The design doc lives at `docs/design.md`. Phase 0 API recon findings are at
`docs/encora-api.md`. Real-API JSON fixtures (one buttonshot per endpoint) live
at `internal/encora/testdata/` — useful when adding tests for the type
definitions or sync logic.

## Roadmap

Phases done:

- ✅ Phase 0 — Encora API recon (docs/encora-api.md)
- ✅ Phase 1 — Repo bootstrap (this commit)

Phases queued (in order):

- Phase 2 — `promptbook sync`: pull /collection + /wants into SQLite. Real
  schema lands here, replacing the placeholder migration. Honor 30-req/min
  rate limit via the `X-RateLimit-Remaining` header on every response.
- Phase 3 — `promptbook rename <path> [--encora-id N] [--dry-run]`:
  configurable folder/file templates with token substitution. Accept encora
  ID from `--flag`, `.encora-id` sidecar, or filename patterns
  (`[encora-N]`, `{e-N}`, `[e-N]`). v1 is explicit-id only; auto-match
  against the synced collection comes in v2.
- Phase 4 — `promptbook nfo <path> [--dry-run]`: walk renamed tree, regex
  encora-id from folder name, fetch detail from local DB, write Jellyfin
  movie.nfo. Cast `<actor><thumb>` URLs come from StageMedia.me (separate
  API key) in v2.
- Phase 5 — `promptbook serve`: echo HTTP, html/template pages, JWT auth
  via OIDC freckle.id JWKS on /api/v1/*. Admin pages for custom poster /
  headshot overrides. Background sync goroutine.
- Phase 6 — Deploy to atlantis cluster under
  `kubernetes/apps/media-tools/promptbook/` (in the
  `nicolerenee/infra` repo). HTTPRoute on public envoy with OIDC
  SecurityPolicy at `promptbook.freckle.media`.
- Phase 7 — Add `/store01/Performances/` as a Jellyfin library and verify
  NFO metadata picks up cleanly.

`docs/design.md` has the full architectural detail and the original design
rationale. The "Status (2026-05-08)" block at the top reflects what's
current.

## Stack

- Go 1.25, modules under `github.com/nicolerenee/promptbook`
- `cobra` (CLI), `viper` (config), `zerolog` (logs)
- `echo` v4 (HTTP server, used by `promptbook serve`)
- `modernc.org/sqlite` (pure-Go SQLite, no CGO)
- `pressly/goose/v3` (migrations, library mode, embedded SQL)
- `coreos/go-oidc/v3` (JWT validation against freckle.id JWKS, planned)

## Layout

```text
main.go                  one-line entry point
cmd/                     cobra subcommands (root, sync, rename, nfo, serve)
internal/
  config/                viper-backed Config struct
  encora/                Encora API client + types
  storage/               sqlite open + embedded goose migrations
    migrations/          *.sql migration files
  sync/                  collection/wants → DB sync logic (planned)
  rename/                template tokens, path planner, mover (planned)
  nfo/                   movie.nfo writer (planned)
  server/                echo server + JWT middleware (planned)
  web/                   html/template + static assets (planned)
build/                   build output
```

## Conventions

- Build output goes to `./build/`. The Taskfile builds with
  `go build -o build/promptbook`.
- Version/Commit/BuildDate/BuiltBy in `cmd` package are set via ldflags by the
  Dockerfile and (eventually) GoReleaser. The `cmd` package owns these globals.
- Strict golangci-lint v2 config lifted from
  [maratori's gist](https://gist.github.com/maratori/47a4d00457a92aa426dbd48a18776322).
  When a lint disagrees with reality, add a precise `//nolint:lintname`
  comment with a one-line reason — never blanket-disable.
- Comments end with periods (godot lint).
- All errors get wrapped with context (`fmt.Errorf("doing X: %w", err)`).
- No package-level globals except cobra command/flag variables and the
  build-time version vars; both are documented with `//nolint:gochecknoglobals`.
- Markdown is linted with `markdownlint-cli2`. Default rules + the project's
  `.markdownlint.yaml`.

## Test conventions

- **Table-driven tests** for any function with multiple input variations.
  Use a `tests := []struct{...}{...}` slice with a `name` field and
  `t.Run(tt.name, ...)`.
- **`github.com/stretchr/testify/require`** for fatal preconditions
  (`require.NoError(t, err)`).
- **`github.com/stretchr/testify/assert`** for non-fatal assertions
  (`assert.Equal(t, want, got)`).
- **`github.com/brianvoe/gofakeit/v7`** for synthetic test data. Seed with
  `gofakeit.Seed(0)` for determinism.
- Real-API JSON fixtures (`internal/encora/testdata/*.json`) for
  shape-fidelity tests against live API responses; gofakeit for synthetic
  data in business-logic tests.

## Encora rate limit

The Encora API is hard-capped at 30 requests per minute. The HTTP client
parses `X-RateLimit-Remaining` from every response. Sync logic must:

- Honor `Retry-After` on 429
- Bail-out (preserving last-good data) when remaining hits the configured
  burst-reserve floor
- Sleep between paginated calls

Don't add code paths that could fan out per-recording detail calls without
checking the rate-limit budget.

## Running locally

```bash
export PROMPTBOOK_ENCORA_APIKEY="$(op read 'op://kube-shared/encora-api/credential')"
task build
./build/promptbook --log-pretty sync
```

## Git

- Author of all commits is the user. Don't add `Co-Authored-By: Claude`.
- Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`,
  `test:`).
