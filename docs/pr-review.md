# PR Review — overnight-phase-2-5

## Summary

Nine code commits plus a docs-only commit on `overnight-phase-2-5`, ~6300
insertions across 60 files. The work delivers Phases 2–5 of the
overnight plan: real schema + collection sync, noun-verb CLI restructure,
the full `library ingest` workflow (parser/template/planner/mover, NFO
writer with golden, subtitle fetcher, ingest orchestrator), and a basic
loopback `serve` (echo + JSON API + embedded HTML/Pico templates). All
hard constraints held: branch is local-only (not pushed, not merged),
no real Encora calls, `--add-to-collection` wired but only ever exercised
through stub clients in tests, and the working tree is clean apart from
an unrelated `tmp/` directory.

Top-line verdict: ship it after a quick triage of the three real concerns
below. Everything builds, lints, and tests cleanly with `-race`. Coverage
is good, the abstractions are reasonable, and the conventions
(Conventional Commits, table-driven tests, testify, gofakeit, no
Co-Authored-By Claude, errors wrapped, package-level globals only where
cobra/version warrant) are followed throughout.

## Acceptance check

| Criterion (from `docs/overnight-plan.md`)                      | Status |
| -------------------------------------------------------------- | :----: |
| Branch `overnight-phase-2-5` exists locally with multiple commits | ✅    |
| `task build` succeeds                                          | ✅    |
| `task test` (`go test -v -race ./...`) succeeds                | ✅    |
| `task lint` succeeds                                           | ✅    |
| `./build/promptbook --help` shows new noun-verb tree           | ✅    |
| `collection --help` / `library --help` show subcommands        | ✅    |
| Tests use table-driven, testify, gofakeit                      | ✅    |
| `docs/design.md` status block updated                          | ✅    |
| `README.md` shows new command surface                          | ✅    |
| `CLAUDE.md` updated with test conventions                      | ✅    |
| Branch not pushed and not merged                               | ✅    |

## Per-phase review

### Phase 2 — collection sync

**Spec fidelity:** matches well.

- Migration 00002 (`internal/storage/migrations/00002_recordings.sql`) drops
  `schema_marker` from 00001 and creates `shows`, `recordings`,
  `cast_entries`, `collection`, `wants`, `sync_runs` with the right
  columns, including `raw_json TEXT NOT NULL` for round-tripping. Cast
  cascade on `recordings` deletion is in place
  (`internal/storage/migrations/00002_recordings.sql:63`).
  `collection` and `wants` also cascade off `recordings`. Booleans use
  `INTEGER NOT NULL DEFAULT 0` as the plan requested.
- Foreign keys are enabled per-connection (`internal/storage/storage.go:39`)
  and the goose migration globals are mutex-protected so parallel test
  cases that hit `storage.Open` don't race
  (`internal/storage/storage.go:22`). Nice catch.
- `internal/sync/sync.go` paginates `Collection` and `Wants` via
  `Page.NextPageURL`, upserts show → recording → cast → collection|wants
  inside a transaction per page, refreshes cast by delete+reinsert (so
  upstream renames don't leave stale rows,
  `internal/sync/sync.go:376`), and writes a `sync_runs` row at start +
  update at finish. The encora client interface is narrowed to a
  test-friendly `Client` interface
  (`internal/sync/sync.go:42`).
- `Sync` always logs run kind `SyncKindAll`. Constants
  `SyncKindCollection`/`SyncKindWants` are declared but unused — see
  Concerns #2.

**Tests:** `internal/sync/sync_test.go` covers four scenarios
(round-trip against the real Marigold fixture, idempotency of two back-to-back
syncs, rate-limit bail at the burst-reserve floor, and the 500-error
path that captures `error_text` in `sync_runs`). Counts match the
fixtures (28 collection + 14 wants). `t.Run` subtests inside the
round-trip share the table-driven shape requested. `require` for setup,
`assert` for shape — by the book.

### Phase 3 — CLI restructure

`cmd/sync.go`, `cmd/rename.go`, `cmd/nfo.go` are gone. `collection.go` /
`library.go` are the parent commands; verbs are split into
`collection_{sync,show}.go`, `library_{ingest,scan,rename,nfo}.go`. Help
output reads cleanly:

```text
promptbook collection sync | show ID
promptbook library ingest SRC | scan PATH | rename PATH | nfo PATH
promptbook serve
```

`collection show ID` (`cmd/collection_show.go:51`) is a pure local-DB
read — no encora client constructed. Distinguishes `ErrRecordingNotFound`
to nudge the user to `collection sync` first.

`cmd/testing.go` exposes `RunForTest` with explicit globals reset so
cobra-driven tests don't leak flag state between runs (good — flag-state
leakage between cobra tests is a classic gotcha).

### Phase 4 — library ingest + rename + nfo

#### `internal/rename/`

- **Parser** (`parser.go`): regex `(?i)[\[\{\(](?:encora|e)-(\d+)[\]\}\)]`
  matches all four pattern forms case-insensitively. Resolver precedence
  is exactly the plan: flag → sidecar → filename → folder name
  (`Resolve()` at line 51). All four resolve sources are tested, plus the
  `case_insensitive` and `wrong_separator` negative paths.
  `[encora-0]` is correctly rejected.
- **Template engine** (`template.go`): smart `{Date}` token formats
  ISO/`January 2006`/`2006` against the partial-date flags, exactly per
  spec. `Sanitize` strips slashes and the null byte and replaces `:` with
  space-dash (so `Marigold Junction` → `Marigold Junction`). Tests cover
  full / month-only / year-only date variants and unknown-token errors.
- **Planner / Mover** (`planner.go`, `mover.go`): `BuildPlan` rejects
  empty templates and an empty library root. `Apply` refuses to overwrite
  an existing dest (`ErrTargetExists`) and falls back to copy+remove on
  cross-device renames. The cross-device detection
  (`isCrossDevice` in `mover.go:89`) walks the LinkError chain and
  string-matches "cross-device" / "EXDEV" — see Nits below for the more
  idiomatic `errors.Is(linkErr.Err, syscall.EXDEV)`.

#### `internal/nfo/`

- `writer.go` emits `<title>`, `<originaltitle>`, `<year>`, `<premiered>`
  (intentionally omitted when `day_known=false` — comment at line 154
  explains why), `<plot>` with HTML stripped, `<set><name>` for the show
  collection grouping, `<tag>` per metadata flag, `<actor>`, and
  `<uniqueid type="encora" default="true">` per spec.
- The Marigold golden lives at `internal/nfo/testdata/recording_pilot.nfo`
  and is checked in. The golden test (`writer_test.go:36`) supports the
  standard `-update` flag pattern. Comparing the file character-for-character
  catches any future drift in `encoding/xml`'s output.
- `tagsOf` (`writer.go:169`) emits `recording_type` first, then
  `master:<x>` only when master differs from recording_type. For Marigold
  both are `pro-shot`, so a single `pro-shot` tag — the golden reflects
  this.

#### `internal/ingest/`

- The orchestrator (`ingest.go`) breaks `ingestOne` into a clean
  pipeline: `resolveID` → `lookupRecording` → `buildPlan` →
  (`fillDryRunPaths` | `applyPlan`). Each step short-circuits on failure
  while preserving `ItemResult`. Error states get a human-readable
  `SkippedReason` plus the underlying `Err`.
- `--add-to-collection` is wired (`Engine.lookupOrAdd` at line 251) and
  always exercised through a `Client` interface. The unit test
  `TestEngineAddToCollectionMockOnly` confirms the mock POST is invoked
  on a missing-from-cache id and returns 99999. **No real Encora write
  endpoint is reachable** unless the user runs the CLI with the flag
  against an unknown id — which the constraints reserve for a follow-up
  session.
- Subtitle download is gated by a `SubtitleFetcher` interface
  (`subtitles.go:20`). Production fetcher is `HTTPSubtitleFetcher` with
  an injected `*http.Client`. Same-language collisions are
  author-disambiguated (`subtitleFilename` in `subtitles.go:102`); spaces
  are normalized to underscores. Tests build a static-body fixture
  server and exercise the multi-author case end-to-end.
- Cross-device move + sidecar write + nfo write all happen post-rename;
  failures are warned-and-continued rather than aborting the rest of the
  walk (`ingest.go:227`).

#### Tests

- `internal/rename/parser_test.go`, `template_test.go`, `planner_test.go`,
  `mover_test.go` — table-driven, testify, gofakeit-fuzzed sanitize.
- `internal/nfo/writer_test.go` — golden + partial/full date shape +
  WriteFile smoke.
- `internal/ingest/ingest_test.go` — five end-to-end scenarios, including
  dry-run, real move, mock-only `AddToCollection`, directory walk, and a
  separate `TestHTTPSubtitleFetcher` that pulls real bytes through the
  production fetcher against an httptest CDN.

#### `library scan|rename|nfo` (4b)

- `library scan` is a thin wrapper: `Engine.Ingest` with `DryRun: true`,
  formatted output (`cmd/library_scan.go:56`).
- `library rename` re-resolves the id, loads the recording from the local
  DB, rebuilds the plan, applies, drops the sidecar (`cmd/library_rename.go`).
- `library nfo` walks a tree, regexes encora-ids out of folder names, and
  rewrites `movie.nfo` from the cache (`cmd/library_nfo.go`). Cache
  misses log a warning but don't abort the walk.
- `cmd/library_scan_test.go` exercises the cobra path end-to-end via
  `cmd.RunForTest` with viper env vars.

### Phase 5 — serve

- Default listen is `127.0.0.1:8080` (`cmd/serve.go:44`). No auth, as
  specified — Phase 5b material.
- API: `/api/v1/{health,recordings,recordings/:id,wants,sync/runs}`. See
  Concerns #1 — `/api/v1/profile` is missing.
- HTML pages: `/`, `/recordings/{id}`, `/wants`, `/sync`. Each page is
  parsed against its own template tree
  (`internal/web/web.go:42`), so the shared `body` block in
  `_layout.html` doesn't collide across pages — exactly the corner the
  plan warned about.
- Embedded static via `go:embed static`; embedded templates via
  `go:embed templates/*.html`. Pico CDN-loaded in `_layout.html`. No
  npm/yarn step.
- `Server.Start` in `internal/server/server.go:77` runs in a goroutine
  with a context-driven shutdown and `ReadHeaderTimeout` set against
  slowloris.
- Tests cover: API health, paginated recordings (default / owned-only /
  wants-only), single-recording lookup (200 / 404 / 400), sync runs JSON,
  page render smoke, and the `Start`-cancels-cleanly path.

## Top concerns

Listed in severity order. None of these are blockers for fast-merge —
they're "would catch this on a real PR".

1. **`/api/v1/profile` not implemented.** The plan explicitly listed
   `GET /api/v1/profile — last-synced profile info`
   (`docs/overnight-plan.md:271`). It is not in `Server.routes()`
   (`internal/server/api.go:37`). The vestigial
   `var _ = encora.Page[encora.Recording]{}` at the bottom of `api.go`
   is justified in a comment as "future profile data" — i.e., a
   conscious decision was made to defer it. Either drop the placeholder
   and the comment, or wire the endpoint. **No `profile` table exists in
   the schema either** — adding the endpoint requires either persisting
   `Profile` from sync (one more `INSERT` in `Sync()`) or returning a
   computed payload from `sync_runs` + counts.
2. **Rate-limit bail miss when `Remaining` is exactly 0.** The bail
   condition is `rl.Remaining > 0 && rl.Remaining <= opts.BurstReserve`
   (`internal/sync/sync.go:138` and again at `:179`). If a server returns
   `X-RateLimit-Remaining: 0`, the `> 0` clause is false, the bail
   doesn't fire, and the next page request 429s. The fixture test passes
   because it sets `remaining=1` against `BurstReserve=2`. Either drop
   the `> 0` guard, or document/test the 0 case explicitly. Probably the
   `> 0` was meant to defend against a header-missing `Remaining=0`
   (which `parseRateLimit` returns when the header is absent), but a
   "header absent" sentinel should be `-1` or a separate bool, not 0.
3. **`Retry-After` header not honored anywhere.** The plan
   (`docs/overnight-plan.md:128`) and `CLAUDE.md` both say sync must
   honor `Retry-After` on 429. The encora client returns
   `ErrRateLimited` on 429 (`client.go:209`) without inspecting the
   header, and `Sync()` simply propagates the error. Today this is
   moot because the burst-reserve bail keeps us above the 429 cliff in
   practice, but the docs lie about the behavior. Either implement the
   header parse + sleep in the client, or amend `CLAUDE.md` to reflect
   the bail-only strategy.

## Nits

Style/taste-level — none worth blocking on.

- `internal/sync/sync.go:25` declares `SyncKindCollection` and
  `SyncKindWants` constants that nothing references. The current code
  always passes `SyncKindAll` to `insertSyncRun`. Either drop the unused
  constants or split `Sync` into per-kind entry points.
- `internal/server/api.go:212` — the `var _ = encora.Page[encora.Recording]{}`
  placeholder reads as scaffolding. If `/api/v1/profile` lands (Concern
  #1) this becomes load-bearing; otherwise drop it and the import.
- `cmd/library_scan.go` doesn't reject `library.root == ""` the way
  `library_ingest.go:76` and `library_rename.go:53` do. Plan-builder
  surfaces a less-pretty error if root is unset. Consistency only.
- `internal/rename/mover.go:89` — `isCrossDevice` does
  `strings.Contains(msg, "cross-device") || strings.Contains(msg, "EXDEV")`.
  More idiomatic: `errors.Is(linkErr.Err, syscall.EXDEV)`. The string
  match works on macOS + Linux today but is fragile.
- `internal/ingest/ingest.go:278` — `plannedSubtitlePaths` hard-codes
  `eng` for the dry-run preview, which is fine as a placeholder but will
  mislead a user with a recording that only has Italian subs. Not worth
  pulling the real subtitle list during dry-run; just be aware.
- `internal/encora/client.go:201` — only `200 OK` is treated as success
  in `doRequest`. `AddToCollection` posts to
  `/collection/{id}/collect` and the recon doc doesn't pin down the
  response status; if encora returns 201/204 the live call will surface
  as `unexpected status`. Mock-tested today, so noticeable only when
  Phase 5b/6 starts using it for real.
- `internal/ingest/ingest.go:264` — when a missing recording is
  AddToCollection-ed, the code returns
  `"recording N added to collection — run sync and re-ingest"`
  rather than auto-resyncing the one record as the plan implied
  ("then re-sync that record"). The deviation is documented in the
  comment at line 270; mock test confirms the POST happened. Worth
  flagging only because the plan wording is slightly more eager.
- `cmd/library_ingest.go:140` — when no API key is set, the code
  constructs an encora client with `APIKey: "unset"` to avoid the empty-
  key error in `encora.New`. If subtitles need fetching, the call lights
  up a 401. Document the "key required for has_subtitles=true" case in
  README, or fail fast in the CLI when both `--add-to-collection` is on
  and the key is missing.
- Two `//nolint:dupl` markers on `syncCollection`/`syncWants`
  (`internal/sync/sync.go:117`, `:158`) — the symmetric structure is
  defensible (different generic types, different writers), but a
  generics pass over the pagination loop would let the dedupe stand.
  Defer.
- `internal/web/web.go:87` — `monthName` is a 12-arm switch; a small
  array indexed by month would read cleaner, but the switch is fine.

## Verification commands run

```text
$ rm -rf .task && task build         # cold rebuild — succeeded
$ task test                           # all packages PASS, cached results shown
$ go test -race ./...                 # all packages PASS under -race
$ task lint                           # 0 issues (golangci-lint v2) + 0 markdown errors
$ ./build/promptbook --help           # noun-verb tree present
$ ./build/promptbook collection --help
$ ./build/promptbook library --help
$ git -C ... config remote.origin.url   # git@github.com:nicolerenee/promptbook.git
$ git -C ... branch -vv                  # main tracks origin/main; overnight-phase-2-5 has no upstream
$ git -C ... log origin/main..HEAD       # all 10 commits unpushed
$ git -C ... log main..HEAD --pretty=full | grep -i "co-author\|claude"
                                         # no Co-Authored-By Claude (only README/CLAUDE filename mentions)
```

`task test` output for context: 9 test packages pass, 60+ subtests, every
one of the named-in-spec tests is present
(`TestSyncBailsOnRateLimitFloor`, `TestSyncFixtureRoundTrip`,
`TestEngineAddToCollectionMockOnly`, `TestWriteMarigoldGolden`,
`TestPagesSmoke`, etc.).
