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
- ✅ Phase 1 — Repo bootstrap
- ✅ Phase 2 — `collection sync`: paginated /collection + /wants into
  SQLite, rate-limit honored, fixture-tested via httptest. Real schema
  in `00002_recordings.sql`.
- ✅ Phase 3 — Noun-verb CLI restructure (collection.{sync,show},
  library.{ingest,scan,rename,nfo}, serve).
- ✅ Phase 4 — `library ingest` workflow + rename + nfo packages.
  Resolver chain (flag/sidecar/filename/folder/interactive), template
  engine with smart partial-date, Plan/Apply mover with cross-device
  fallback, Jellyfin movie.nfo writer with golden test, subtitle
  fetcher behind an interface.
- ✅ Phase 5 — `promptbook serve` (basic). Echo HTTP, JSON `/api/v1/*`,
  HTML pages via embedded html/template + Pico CSS. Listens on
  `[::]:8080` (all interfaces) by default; set `server.listen` /
  `PROMPTBOOK_SERVER_LISTEN` to `127.0.0.1:8080` to restrict to
  loopback. JWT/OIDC arrives in Phase 5b.

Phases queued (in order):

- Phase 5b — JWT/OIDC on /api/v1/* via freckle.id JWKS. Admin pages for
  custom poster / headshot overrides.
- Phase 6 — Deploy to atlantis cluster under
  `kubernetes/apps/media-tools/promptbook/` (in `nicolerenee/infra`).
  HTTPRoute on public envoy with OIDC SecurityPolicy at
  `promptbook.freckle.media`.
- Phase 7 — Add `/store01/Performances/` as a Jellyfin library and
  verify NFO metadata reads cleanly.

`docs/design.md` has the full architectural detail. The "Status
(2026-05-09)" block at the top reflects what's current.

## Stack

- Go 1.25, modules under `github.com/nicolerenee/promptbook`
- `cobra` (CLI), `viper` (config), `zerolog` (logs)
- `echo` v4 (HTTP server, used by `promptbook serve`)
- `modernc.org/sqlite` (pure-Go SQLite, no CGO)
- `pressly/goose/v3` (migrations, library mode, embedded SQL)
- `coreos/go-oidc/v3` (JWT validation against freckle.id JWKS, planned)
- `golang.org/x/image` (pure-Go font rendering for the burned-in
  backdrop renderer; embeds DejaVu Serif Bold + Regular, licensed
  under the Bitstream Vera Fonts license, see
  `internal/imagerender/assets/LICENSE`)

## Layout

```text
main.go                  one-line entry point
cmd/                     cobra subcommands
  root.go                root command + viper config + logging setup
  collection.go          parent for collection.* verbs
  collection_sync.go     `promptbook collection sync`
  collection_show.go     `promptbook collection show ID`
  library.go             parent for library.* verbs
  library_ingest.go      `promptbook library ingest SRC`
  library_scan.go        `promptbook library scan PATH`
  library_rename.go      `promptbook library rename PATH`
  library_nfo.go         `promptbook library nfo PATH`
  library_watch.go       `promptbook library watch`
  library_queue.go       `promptbook library queue`
  serve.go               `promptbook serve`
  testing.go             RunForTest helper (not for production code)
internal/
  config/                viper-backed Config struct
  encora/                Encora API client + types + httptest fixtures
  storage/               sqlite open + LoadRecording + ParseRecordingID
    migrations/          *.sql migration files (00001 init, 00002 real schema)
  sync/                  collection/wants → DB sync (rate-limit aware)
  rename/                parser + template engine + planner + mover
  nfo/                   Jellyfin movie.nfo writer + golden testdata
  ingest/                full ingest orchestration + subtitle downloader
  server/                echo server + handlers (api.go, pages.go)
  web/                   html/template + static assets (embedded)
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
./build/promptbook --log-pretty collection sync
./build/promptbook --log-pretty library scan ~/incoming
./build/promptbook --log-pretty library ingest ~/incoming --dry-run
./build/promptbook --log-pretty serve   # http://127.0.0.1:8080
```

## Git

- Author of all commits is the user. Don't add `Co-Authored-By: Claude`.
- Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`,
  `test:`).
