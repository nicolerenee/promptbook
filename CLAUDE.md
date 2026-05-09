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
