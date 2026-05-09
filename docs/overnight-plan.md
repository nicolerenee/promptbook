# Overnight Build Plan — 2026-05-08

This brief is for an autonomous agent run. Read it top-to-bottom and execute.
Stop and report back when complete or genuinely stuck. Don't deviate from
constraints.

## Repo

`github.com/nicolerenee/promptbook` at `/Users/nicole/repos/github.com/nicolerenee/promptbook`.
All git operations: use `git -C` with absolute paths so cwd doesn't matter.

## Branch

Work on a single branch: `overnight-phase-2-5`. Create from current `main`.
Multiple logical commits on this branch are fine — one commit per phase or
sub-phase, whatever is natural. Each commit must build, lint, and test
cleanly. Don't merge to main; don't push anywhere; don't open a PR.

## Hard constraints (non-negotiable)

- **No `git push`.** Anywhere. Ever.
- **No merge to `main`.** Leave the branch for the user to ff-merge in the morning.
- **No real Encora API access.** No API key is provisioned. All sync/ingest
  logic is fixture-tested via `httptest.Server` against the JSON in
  `internal/encora/testdata/`.
- **No touching the user's real `~/store01/Performances/` tree** or the
  `nicolerenee/infra` repo.
- **No adding to the user's encora collection or wants.** The
  `--add-to-collection` flag in `library ingest` must be wired and unit-tested
  against a mock client, but never invoke the real Encora write endpoints.
- **No subtitle downloads from real encora.** Mock-tested only.
- **Each commit builds, lints, and tests.** Run `task lint` and `task test`
  before every commit. If lint flags something the right call is genuinely
  not to fix it, add a precise `//nolint:lintname // one-line reason` rather
  than blanket-disabling.

## Conventions

- Go 1.26, modules under `github.com/nicolerenee/promptbook`
- Conventional Commits (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `chore:`)
- **No `Co-Authored-By: Claude` lines.** Author of all commits is the user.
- Strict `golangci-lint` v2 config (already configured at `.golangci.yml`).
  Comments end with periods (`godot`). Errors wrapped:
  `fmt.Errorf("doing X: %w", err)`.
- No package-level globals except cobra command/flag variables and the
  build-time version vars. Document with `//nolint:gochecknoglobals`.

### Test conventions (NEW — apply throughout)

- **Table-driven tests** for any function with multiple input variations.
  Use a `tests := []struct{...}{...}` slice and `t.Run(tt.name, ...)`.
- **`github.com/stretchr/testify/require`** for fatal preconditions
  (`require.NoError(t, err)`, `require.NotNil(t, x)`).
- **`github.com/stretchr/testify/assert`** for non-fatal assertions
  (`assert.Equal(t, want, got)`, `assert.True(t, ok)`).
- **`github.com/brianvoe/gofakeit/v7`** for fake data (`gofakeit.Name()`,
  `gofakeit.UUID()`, etc.). Seed with `gofakeit.Seed(0)` in test setup for
  determinism.
- Real encora response fixtures (`internal/encora/testdata/*.json`) for
  shape-fidelity tests against live API responses; gofakeit for synthetic
  data in business-logic tests.

Add the deps as part of Phase 2's setup commit:

```bash
cd /Users/nicole/repos/github.com/nicolerenee/promptbook
go get github.com/stretchr/testify
go get github.com/brianvoe/gofakeit/v7
go mod tidy
```

## Phase order + deliverables

### Phase 2: `promptbook collection sync`

**Goal:** Pull `/api/collection` and `/api/wants` from Encora into local SQLite.

**Schema** (`internal/storage/migrations/00002_recordings.sql`): drops the
placeholder `schema_marker` table from migration 00001 and replaces with the
real schema. Tables (refine column types as needed):

- `shows` (show_id PK, name, description_html, last_seen_at)
- `recordings` — one row per recording. Include all metadata fields used
  later by rename/nfo. Store the full encora JSON blob as `raw_json TEXT NOT
  NULL` for round-tripping. Foreign key to `shows`.
- `cast_entries` — many-per-recording (performer + character + status).
  Cascade-delete on recording removal.
- `collection` — owned recordings (1:1 subset of `recordings`). Includes
  `format`, `user_notes`, `user_watched`, `collected_at`, `last_synced_at`.
- `wants` — wanted recordings (1:1 subset). The wire format only carries the
  recording payload — no priority/added_at. Just `(recording_id, last_synced_at)`.
- `sync_runs` — log of sync invocations with kind, started_at, finished_at,
  ok_count, error_count, rate_limit_remaining at end, error_text.

Use `INTEGER NOT NULL DEFAULT 0` for booleans. Use `DATETIME` for timestamps
(SQLite stores as TEXT but it documents intent).

**Encora client** is already done — `Profile`, `Collection(page)`,
`Wants(page)` methods exist on `Client` (`internal/encora/client.go`),
returning `(Page[T], RateLimitInfo, error)`.

**Sync logic** (`internal/sync/sync.go`):

- `Sync(ctx, client, db) (*SyncResult, error)` — one entry point.
- Paginate `Collection` and `Wants` via `Page.NextPageURL` until exhausted.
- For each `CollectionEntry`, upsert show → recording → cast_entries →
  collection row. For each `WantEntry`, upsert show → recording → wants row.
- Honor rate limit: after each request, if `RateLimitInfo.Remaining <=
  config.Encora.RateLimit.BurstReserve`, sleep until the next minute window
  (`time.Until(time.Now().Truncate(time.Minute).Add(time.Minute))`).
- Wrap each page's writes in a transaction.
- Write a `sync_runs` row at start; update at end.
- Return `SyncResult` with counts (collection_count, wants_count, errors).

**CLI** (`cmd/sync.go` becomes `cmd/collection_sync.go`, restructured under
the noun-verb pattern below).

**Tests:**

- `internal/sync/sync_test.go` — table-driven. Set up `httptest.Server`
  serving the `testdata/*.json` fixtures. Construct a Client pointed at the
  server. Call `Sync()`. Assert correct row counts, that the Marigold recording
  (id 90100222) ends up in `recordings` with the right fields, that cast_entries
  has the right number of rows, etc. Use `require.NoError` for setup,
  `assert.Equal` for shape checks.
- Test the rate-limit-pause behavior with a fixture server that returns
  `X-RateLimit-Remaining: 0` and check that Sync sleeps (or returns early
  with state preserved — your choice, but document it).

### Phase 3: command surface restructure (under `collection` and `library`)

Reorganize cobra commands into the noun-verb structure:

```text
promptbook collection sync
promptbook collection show ID
promptbook library ingest SRC
promptbook library scan PATH
promptbook library rename PATH
promptbook library nfo PATH
promptbook serve
```

Layout:

```text
cmd/
  root.go                 # unchanged
  collection.go           # the parent collection cobra command
  collection_sync.go
  collection_show.go
  library.go              # the parent library cobra command
  library_ingest.go
  library_scan.go
  library_rename.go
  library_nfo.go
  serve.go                # see Phase 5
```

The old flat `sync.go`, `rename.go`, `nfo.go` are removed.

`collection show ID` reads from local DB, prints recording detail (cast,
master, date, format if owned, etc.). Pure read; no encora.

### Phase 4: `library ingest` — the big workflow command

**Goal:** Point at a directory, ingest each video into the canonical library
in one command.

**Algorithm** for each video file or folder-with-video found under SRC:

1. **Resolve encora-id** in this order:
   - `--encora-id N` flag (only valid for single-file SRC)
   - `.encora-id` sidecar file in the folder (bare integer ID inside)
   - Filename/folder pattern parser: accept `[encora-N]`, `{e-N}`, `[e-N]`,
     `(encora-N)` (case-insensitive)
   - If `--interactive` and the above all failed: print proposed parsed
     fields (best-effort show/tour/date guess) and prompt stdin for an ID.
     Empty input = skip this entry.
   - Otherwise: log warning, skip (or move to a `_unmatched/` subdir of
     `library.root` — implementer's call, document the choice).
2. **Look up** the recording in local DB. If not present, log warning. Two
   options behind `--add-to-collection`:
   - Off (default): skip, log "not in DB, --add-to-collection would fix"
   - On: would POST to `/collection/{id}/collect`, then re-sync that record.
     **Implement and unit-test this with a mock client** but do NOT call the
     real encora endpoint during the agent run (no key anyway).
3. **Compute canonical name** via `internal/rename/template.go` token engine:
   - `{Show}`, `{Tour}`, `{Master}`, `{EncoraID}`
   - `{Date}` — smart token: ISO `2024-01-21` when full; "December 2009" when
     `day_known=false`; "2009" when `month_known=false`
   - `{DateUS}`, `{DateNumeric}`, `{Year}`, `{Format}`, `{Variant}`
   - Templates come from `config.Library.FolderTemplate` and
     `config.Library.FileTemplate`.
4. **Move** the file into `{library.root}/{folder}/{file}.{ext}`.
   - Use `os.Rename` (handles intra-device); fall back to copy+remove for
     cross-device. Don't overwrite — error out and log if dest exists.
5. **Subtitles**: if `recording.metadata.has_subtitles` is true, fetch
   `/recording/{id}/subtitles`, download each `.url` to
   `{folder}/{file}.{lang}.srt` (or `.<author>.srt` if multiple per language).
   **Mock-test this — no real encora calls.**
6. **NFO**: write `{folder}/movie.nfo` via the writer in
   `internal/nfo/writer.go` (Phase 4 stage). Jellyfin-flavored XML with
   title, year, premiered, plot, tags, `<set><name>{Show}</name></set>` for
   show-grouping, `<actor>` per cast entry, `<uniqueid type="encora"
   default="true">{ID}</uniqueid>`.

**Flags:**

- `--dry-run` — print the full plan (resolved IDs, proposed paths,
  subtitle/NFO actions) but make no changes.
- `--interactive` — prompt for missing IDs.
- `--add-to-collection` — wired but mock-only during this run.
- `--encora-id N` — single-file mode.

**Layout:**

```text
internal/
  rename/
    parser.go         # encora-id extraction from filenames/sidecars
    template.go       # token engine
    planner.go        # build a RenamePlan from a recording + path
    mover.go          # apply a RenamePlan
  nfo/
    writer.go         # encoding/xml NFO writer
  ingest/
    ingest.go         # orchestrates the full workflow
    interactive.go    # stdin prompts (mock-friendly via io.Reader)
    subtitles.go      # subtitle downloader (uses encora.Client)
```

**Tests** (table-driven, testify, gofakeit where appropriate):

- `internal/rename/parser_test.go` — table of input-name → expected-id pairs
  covering all formats. Use gofakeit for synthetic show/tour names.
- `internal/rename/template_test.go` — date formatting behavior across
  full/month-only/year-only date variants; token substitution for all tokens.
- `internal/rename/planner_test.go` — build plans against fixture recordings.
- `internal/rename/mover_test.go` — actual `os.Rename` against `t.TempDir()`.
- `internal/nfo/writer_test.go` — golden test: build NFO from the Marigold
  recording fixture (`testdata/recording_8222.json`), compare to a checked-in
  golden file at `internal/nfo/testdata/recording_8222.nfo`. (Generate the
  golden once and check it in.)
- `internal/ingest/ingest_test.go` — end-to-end with a `t.TempDir()` source +
  dest, an httptest server with the encora fixtures, an in-memory SQLite
  pre-seeded with the Marigold recording. Verify file moves to canonical path,
  subtitles downloaded (mock), NFO written.

### Phase 4b: low-level commands

`library scan PATH` — runs the ingest planner in dry-run mode and prints a
report (matched/unmatched, proposed renames, missing subtitles). No I/O.

`library rename PATH` — rename only. No move, no subs, no NFO. Useful for
fixing a folder that's already in the library but with the wrong scheme.

`library nfo PATH` — NFO regen only. Walks tree, regenerates NFOs from DB.

These are thin wrappers over the same `internal/rename` and `internal/nfo`
packages. Each gets a small test demonstrating it operates correctly via the
cobra command path.

### Phase 5: `promptbook serve` (basic, no auth)

**Scope (intentionally minimal):**

- Echo v4 HTTP server. Default listen `127.0.0.1:8080` (localhost-only). No
  auth. JWT/OIDC is **deferred to a later session**.
- JSON API:
  - `GET /api/v1/profile` — last-synced profile info
  - `GET /api/v1/recordings?limit=&offset=&owned=true|false` — paginated
  - `GET /api/v1/recordings/{id}` — full detail incl. cast
  - `GET /api/v1/wants` — list of wants
  - `GET /api/v1/sync/runs` — sync history
  - `GET /api/v1/health` — liveness
- HTML pages, served via embedded `internal/web/static/` and
  `internal/web/templates/`:
  - `/` — collection grid
  - `/recordings/{id}` — recording detail
  - `/wants` — wants list
  - `/sync` — sync run log
- Frontend: vanilla HTML/CSS/JS. Fetch the API. **No npm/yarn/build step.**
  Match the seedreap pattern at `~/repos/github.com/seedreap/seedreap/ui/`
  (you may inspect that repo for shape — the user has it locally). Lift CSS
  framework choice from there (e.g., Pico/Bulma/etc., whatever seedreap uses
  via CDN).

**Layout:**

```text
internal/
  server/
    server.go         # echo bootstrap, route registration
    middleware.go     # logging
    api.go            # /api/v1/* handlers
    pages.go          # HTML page handlers
  web/
    static/           # CSS, JS, embedded
    templates/        # html/template, embedded
```

**Tests:**

- `internal/server/api_test.go` — table-driven against an echo testserver.
  Seed an in-memory SQLite. Assert response shapes.
- `internal/server/pages_test.go` — render each page template and assert it
  contains expected text (smoke-level only).

## Out of scope for this run

- JWT / OIDC auth on `/api/v1/*` (Phase 5b, separate session)
- `collection list`, `collection search` commands
- Real encora writes (`--add-to-collection` is wired + mock-tested only)
- Real subtitle downloads (mocked only)
- Deploy to atlantis (Phase 6, separate session)
- Custom poster/headshot upload UX (Phase 5b material)

## Acceptance — what "done" looks like

- Branch `overnight-phase-2-5` exists locally with multiple commits.
- `task build` succeeds.
- `task test` succeeds (`go test -v -race ./...`).
- `task lint` succeeds.
- `./build/promptbook --help` shows the new noun-verb command tree.
- `./build/promptbook collection --help`, `library --help`, etc. each show
  their sub-commands.
- All test files use table-driven structure where multiple cases apply,
  testify assertions, and gofakeit for synthetic data.
- `docs/design.md` status block updated to reflect what shipped.
- `README.md` shows the new command surface.
- `CLAUDE.md` is updated with the test conventions section.
- The branch is **not pushed** and **not merged** — both are the user's call.

## Stuck-protocol

If you hit a genuine blocker (e.g., a fixture file is missing or
contradictory, or a constraint is impossible to satisfy without breaking
another), **stop**, leave the work in a consistent state on the branch with
a `WIP:` commit, and report what's blocking. Don't try to plow through.

## Final report format

When done, summarize:

1. Branch name + commit count + final HEAD SHA
2. Per-phase: what shipped, file list, test count, any deviations from spec
3. End-to-end commands the user should run in the morning to verify (with
   their encora API key set as `PROMPTBOOK_ENCORA_APIKEY`)
4. Anything you'd flag for review

Brevity > completeness in the summary; detail lives in the diff.
