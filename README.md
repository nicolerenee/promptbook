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

- `promptbook sync` — pull collection + wants from Encora into the local cache
- `promptbook rename <path>` — rename a recording on disk into the canonical scheme
- `promptbook nfo <path>` — write Jellyfin `movie.nfo` files alongside videos
- `promptbook serve` — run the catalog HTTP server

All commands share `--config`, `--log-level`, `--log-pretty`. Subcommands that
modify the filesystem support `--dry-run`.

## Folder/file naming

Configurable via `library.folderTemplate` and `library.fileTemplate`. Tokens:

- `{Show}`, `{Tour}`, `{Master}`, `{EncoraID}`
- `{Date}` (ISO `2024-01-21`), `{DateUS}`, `{DateNumeric}`, `{Year}`
- `{Format}`, `{Variant}`

Default scheme:

```text
{Show} - {Tour} - {Date} [encora-{EncoraID}]/
  {Show} - {Tour} - {Date} [{Master}].mp4
```

## Config

YAML file searched at `$HOME`, current directory, and `/config`. See
`config.example.yaml`. Any value can be overridden via `PROMPTBOOK_*` env vars.

## Build

```bash
task build
./build/promptbook --version
```
