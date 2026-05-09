# promptbook

Encora-backed catalog and library tooling for Broadway recordings.

A *promptbook* is the stage manager's annotated master script — the canonical
reference for every cue, blocking, and scene change in a production. This tool
plays the same role for your home recording library: it mirrors your
[Encora](https://encora.it) collection into a local SQLite cache, renames
recordings on disk into a canonical scheme that bakes the Encora ID into the
folder name (mirroring how Radarr bakes `[tmdbid-...]`), and generates
Jellyfin-compatible NFO metadata so your library reads clean without bouncing
through TMDB.

## Subcommands

```text
promptbook collection sync         # pull /collection + /wants into local cache
promptbook collection show ID      # print recording detail from local cache

promptbook library ingest SRC      # full pipeline: resolve → rename → subs → NFO
promptbook library scan PATH       # dry-run report; matches/unmatched/destinations
promptbook library rename PATH     # rename only (no move/subs/NFO)
promptbook library nfo PATH        # walk a tree and rewrite movie.nfo from cache
promptbook library watch           # poll incomingDirs and enqueue files for review
promptbook library queue           # list files waiting for manual import

promptbook serve                   # HTTP UI + JSON API on 127.0.0.1:8080
```

All commands share `--config`, `--log-level`, `--log-pretty`. Subcommands that
modify the filesystem support `--dry-run`. `library ingest` adds `--encora-id`,
`--interactive`, and `--add-to-collection`.

## Folder/file naming

Configurable via `library.folderTemplate` and `library.fileTemplate`. Tokens:

- `{Show}`, `{Tour}`, `{Master}`, `{EncoraID}`
- `{Date}` smart partial-date: ISO `2024-01-21` when full,
  `December 2009` when only month is known, `2009` when only year
- `{DateUS}`, `{DateNumeric}`, `{Year}`, `{Format}`, `{Variant}`

Default scheme:

```text
{Show} - {Tour} - {Date} [encora-{EncoraID}]/
  {Show} - {Tour} - {Date} [{Master}].mp4
  movie.nfo
  .encora-id           # sidecar with the bare ID for re-runs
  *.eng.srt            # one per language; <author> suffix on collisions
```

Encora ids in source filenames are resolved in this order: `--encora-id`
flag → `.encora-id` sidecar in the parent folder → recognized patterns
in the filename → patterns in the parent folder name → interactive
prompt (when `--interactive` is on). The recognized patterns
(case-insensitive) are `[encora-NNN]`, `{e-NNN}`, `[e-NNN]`, and
`(encora-NNN)`.

## Config

YAML file searched at `$HOME`, current directory, and `/config`. See
`config.example.yaml`. Any value can be overridden via `PROMPTBOOK_*`
env vars (e.g. `PROMPTBOOK_ENCORA_APIKEY`, `PROMPTBOOK_LIBRARY_ROOT`).

## Build

```bash
task build
./build/promptbook --version
```

## HTTP UI

`promptbook serve` exposes:

- `/` collection grid (filterable owned/wants/all)
- `/recordings/{id}` detail page with cast + notes
- `/wants` wishlist
- `/sync` recent sync run log

JSON API under `/api/v1/{health,recordings,recordings/{id},wants,sync/runs}`.
Auth (JWT/OIDC) is deferred to a follow-up phase — the default listen
address is loopback-only.
