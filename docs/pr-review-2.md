# PR Review #2 — overnight-phase-2-5 (post-Wave 7)

## Summary

51 logical commits (62 with merges) on top of the previous review's HEAD
(`6dc1aed`), ~9.2k insertions across 74 files. The branch lands the
status-reconciliation taxonomy (`recording_versions` + `RecordingState`),
first-class `performers`/`characters` tables, an append-only `history`
log, a single-row `profile` cache, a `manual_import_queue` plus the
NFS-friendly polling scanner that fills it, a real StageMedia client,
five new server pages (people list/detail, queue, history, mismatches),
and the first production code path that pushes changes back to Encora
(`/apply`). Every package builds, `task lint` reports 0 issues, every
test passes under `-race`, and the branch has no upstream — confirmed
unpushed.

Top-line verdict: ship-able, but not without a triage pass first. The
apply workflow is well-isolated and safety-engineered (sequential
calls, success-only history, 503 when no client, no destructive
endpoints), but two real concerns deserve eyes before any of it runs
against live Encora: the listen address is `[::]:8080` (all
interfaces) when the docs and `cmd/serve.go` claim loopback-only — a
problem that pre-dates this batch but becomes load-bearing now that
write endpoints exist; and `Retry-After` is parsed but still not
honored by sync/apply (the previous concern #3 only got half-fixed).
Everything else is polish.

## Compared to the previous review

| Concern (PR #1)                                             | Status |
|-------------------------------------------------------------|--------|
| #1 `/api/v1/profile` not implemented                         | Resolved — `internal/server/api.go:71`, profile table at `00007_profile.sql`, sync persists at `internal/sync/sync.go:128`. |
| #2 Rate-limit bail miss when `Remaining` exactly 0           | Resolved — `> 0` guard dropped at `internal/sync/sync.go:186` & `:227`; `TestSyncBailsOnRateLimitFloor` now exercises both `remaining=1` and `remaining=0`. |
| #3 `Retry-After` not honored anywhere                        | Half-fixed — header now parses (`internal/encora/client.go:284-290`) and is exposed on `RateLimitInfo`; nothing actually sleeps on it. `internal/server/apply.go:124-128` surfaces a generic "rate limited; retry later" without the duration. |

Three previously-flagged nits are also addressed: the `var _ = encora.Page{}`
placeholder is gone, encora's `doRequest` accepts 201/204 (commit
`5fea9e1`), and `monthName` was untouched — that one's still a 12-arm
switch. All others are deferred (acceptable).

## Acceptance check

| Gate                                                       | Result |
|------------------------------------------------------------|:------:|
| `task build` (cold, then warm)                             | OK     |
| `task lint` (golangci-lint v2 + markdownlint)              | 0 issues |
| `task test` (go test -v -race ./...)                       | 10 pkgs PASS, 134 test funcs |
| `go test -race ./...` (independent invocation)             | PASS   |
| `go vet ./...`                                              | clean  |
| Branch unpushed (no upstream tracking)                      | OK     |
| No `Co-Authored-By: Claude` lines in commits                | OK     |
| Conventional Commit prefixes throughout                     | OK     |
| README + design.md status updated for new commands          | OK (`docs/design.md:6` "Status (2026-05-09)") |
| README documents `library watch` / `library queue`          | OK (`README.md:24-25`) |
| Real Encora API never invoked from tests                    | Verified — every test uses `httptest.Server` |
| `EncoraWriteClient` interface excludes destructive verbs    | OK (`internal/server/apply.go:21`) |

## Per-area review

### Storage / schema

Five new migrations (`00003`-`00007`), each matching its purpose:

- `recording_versions` (`internal/storage/migrations/00003_recording_versions.sql`)
  has `(recording_id, file_path)` unique index so the on-disk inventory
  is idempotent across rescans. Cascade deletes from `recordings`. Tests
  cover insert, idempotent re-upsert (preserves `added_at`), cascade,
  and explicit deletes.
- `performers` + `characters` (`00004`) lift the denormalized cast columns
  into first-class tables. The sync writer keeps them in step inside
  the same transaction as the `cast_entries` write
  (`internal/sync/sync.go:430-491`). The denormalized columns on
  `cast_entries` are still populated — annotated as legacy in
  `refreshCastEntries`'s comment block — so consumers can migrate
  gradually. `TestSyncPopulatesPeopleTables` proves Avery Morrison
  (id 90001001) and Marigold (character id 90002001) round-trip.
- `history` (`00005`) is a generic event log keyed by kind + optional
  `recording_id` + JSON details column. `internal/storage/history.go`'s
  `RecordEvent` defaults `OccurredAt` and `Details` so callers stay
  terse. `ListHistory` supports kind, recording-id, time-range, and
  pagination. `PruneHistoryOlderThan` is in place but not wired into
  any retention loop yet — fine for now.
- `manual_import_queue` (`00006`) has the right shape: a
  `file_path UNIQUE` so the scanner is idempotent, a `discovered_at`
  preserved on conflict update, and nullable `suggested_recording_id`.
  `EnqueueFile`'s id-resolve dance after upsert
  (`internal/storage/import_queue.go:67-93`) is over-defensive on
  SQLite (the `LastInsertId` reliably returns the affected rowid for
  upserts) but harmless.
- `profile` (`00007`) is a single-row cache (`CHECK (id = 1)`).
  `LoadProfile` returns `ErrProfileNotSynced` when empty so the API
  can 404 with a useful hint
  (`internal/server/api.go:101-112`).

`storage.RecordingState` + `ComputeStatus` correctly implement the
five-status taxonomy. The truth table (`internal/storage/state.go:71-92`)
matches the user's stated intent: format equality is gating between
Synced and FormatMismatch (line 74); collection takes precedence over
wants when both apply (lines 74-76 → 80, with the wants-only "synced"
branch deliberately falling through; covered by table case
`collection wins over wants when both set + file + match` in
`state_test.go:121-129`). One subtlety worth flagging: the orphan
fallback case (no file, neither collection nor wants) lands in the
default branch on line 89 — defensible, but in practice no row would
exist to compute against.

`ListStates` (line 158) uses a UNION over the three contributing
tables and an N+1 `ListVersions` per row to compute LocalFormat. The
docstring notes the perf characteristic and the alternative; reasonable
for a library that maxes out around the user's 28 owned + 14 wanted.

### Sync + Encora client

Concern #2 fix at `sync.go:186` (and mirror at `:227`) is correct
and tested with both `remaining=1` and `remaining=0` table cases.

Profile fetch at the start of `Sync` is best-effort
(`sync.go:95-97`) — a 401/network failure logs a warning and
collection/wants continue. `syncProfile` writes through
`storage.UpsertProfile` which persists the `last_synced_at` for the UI.

The encora client gained five new write methods: `AddToCollection`,
`UpdateCollectionFormat`, `UpdateCollectionNotes`, `RemoveFromCollection`,
`AddToWants`, `RemoveFromWants` (`client.go:158-197`). All POSTs
correctly URL-PathEscape user-controlled fragments
(`UpdateCollectionFormat` at line 166, `UpdateCollectionNotes` at line
176). `doRequest` accepts 200/201/204 as success
(`client.go:243-246`). `RateLimitInfo` now includes `RetryAfter` parsed
from the integer-seconds form of the header. Every write endpoint has
a corresponding test against an `httptest.Server` covering at minimum
the 200-OK happy path and the 401/429 error branches.

`PersistRecording` (in `internal/sync/persist.go`) is the new helper
that lets ingest auto-fetch a recording into the local DB without a
full sync. Cleanly factored — uses the same `upsertRecording` the sync
loop does, scoped to a single transaction. No cast write happens here
(it's nested in `upsertRecording`), but neither does any
collection/wants write — exactly the orphan semantics the spec wanted.

### Ingest + scanner

`Engine.lookupOrAdd` (`internal/ingest/ingest.go:398-429`) is the
right shape:

1. Try `LoadRecording` — if hit, return it as not-fresh.
2. If `ErrRecordingNotFound`, fall through to `Client.Recording(ctx, id)`.
3. On `encora.ErrNotFound`, return a clear "doesn't exist in Encora"
   error.
4. On success, `PersistRecording` lands an orphan row in shows +
   recordings + cast_entries.
5. If `opts.AddToCollection`, additionally POST to
   `/collection/{id}/collect`.

The `AutoAdded` flag on `ItemResult` propagates into history details
(`auto_added: true` key) so the UI/CLI can phrase the summary
correctly. Tests `TestEngineIngestAutoAddsUnknownID` and
`TestEngineIngestAutoAddsUnknownIDWithCollection` cover both cases
exactly.

`recordVersion` (`ingest.go:268-299`) writes a `recording_versions`
row on every successful move. Codec/quality are sniffed from the
**source** filename (line 279), not the target — important call-out
in the comment, since the target name is template-driven and
template-stripped of release tags.

`recordIngestEvent` (`ingest.go:312-336`) skips dry-runs and
"no encora id" walk-of-text noise. Other skips and all moves get a
history row. `ingestEventDetails` builds a stable-keyed map.

Polling scanner (`internal/scanner/scanner.go`) is exactly the spec:

- `Run` runs an immediate first pass then a `time.Ticker` loop.
- `Scan` reconciles missing entries first, then walks every WatchDir.
- `processFile` checks `recording_versions` first via
  `versionExistsForPath` to silently skip ingested files.
- `recordError` caps per-pass error count at 10 to defend against an
  NFS storm.
- `confidenceFor` queries the local DB to assign `high`/`low`
  confidence based on whether the encora id is known.

Tests cover enqueue (resolved-known, resolved-unknown, unresolved),
silent skip of ingested files, removal of vanished files, and the
no-id case. Logger silenced via `zerolog.Nop()` so test output stays
focused. No goroutines outside `Engine.Run` — all the surfaces tests
exercise are pure-over-(db, fs).

### Server / API

Routes consolidated to `internal/server/api.go`'s `routes()` with all
new endpoints:

- `GET /api/v1/profile` — concern #1 from PR #1 resolved.
- `GET /api/v1/queue` — manual import queue list.
- `GET /api/v1/people` + `GET /api/v1/people/:id`.
- `GET /api/v1/history` (kind/limit/offset filters).
- `GET /api/v1/mismatches` (type filter).
- `POST /api/v1/apply`.

HTML pages: `/queue`, `/people`, `/people/{id}`, `/history`,
`/mismatches`, `/apply` (POST + redirect-on-GET).

`loadStatefulRecordings` (`api.go:239`) is the new home-page query
path, ranging the reconciler output and decorating with show/tour/date
in a single batch query. The legacy `loadRecordingsList` is deprecated
in comments but still backs `/api/v1/wants` and `/wants` —
intentionally kept for now. Worth a follow-up to migrate those too,
but not blocking.

`paramInt` uses `json.Unmarshal` to parse a query int (`api.go:453`).
Quirky — `strconv.Atoi` is the obvious choice — but works.

People handlers (`internal/server/people.go`) are well-shaped:
`fetchHeadshot` is gated by `s.Stagemedia() != nil`, scoped to the
first recording's show id, and time-boxed at 5 seconds
(`stagemediaImageTimeout`). A stagemedia outage logs at `Debug` and
returns an empty headshot URL — the page still renders.

Recording detail page (`pages.go:121-169`) has the same time-boxed
poster fetch (`fetchPosters` line 174-190) and a clean NFT warning
helper (`nftWarning` line 195-212) that handles "forever", future-date,
past-date, and unparseable variants. A small thoroughness win: the
unparseable-date path (line 205) surfaces the raw string instead of
silently dropping the warning.

### UI templates + CSS

`internal/web/web.go` registered all new templates by name in
`pageNames`. Each gets its own parsed tree to avoid block-name
collisions. New helpers `firstPoster` and `humanSize` are used; the
existing `smartDate` matches the rename engine's `{Date}` token
behavior (4-digit year, "January 2009" for month-only, ISO when full).

`mismatches.html` (`internal/web/templates/mismatches.html`) is the
biggest template addition. The form encodes `action[i].type`,
`action[i].recording_id`, and (only for format_mismatch rows)
`action[i].new_format` — exactly the shape `parseApplyForm` decodes
on the server. Checkboxes are absent for `missing_file` and
`wanted_file` rows (line 39-47) since those are flag-only; instead a
"—" placeholder renders. The submit button is JS-disabled until at
least one row is checked.

`apply_result.html` cleanly separates per-row OK/Error rendering and
links back to /mismatches and /history. No per-error stack-trace
exposure beyond the `describeEncoraError` mapped string — good.

### Apply workflow (highlighted because it's the first prod-path Encora write)

`internal/server/apply.go` is the new prod-path code. Every safety
property the brief asked for holds:

1. **Gate when no encora client**: `s.encora == nil` ⇒ 503 with body
   "encora client not configured" (lines 173-176, 220-222). Tested
   by `TestAPIApplyWithoutEncoraClient`.
2. **Sequential, not concurrent**: the `for _, action := range
   req.Actions { results = append(...) }` loop at lines 184-187 (and
   sibling at 236-238) issues the calls in series. No `go func()`
   anywhere. The 30-req/min Encora ceiling is respected by virtue of
   not being challenged.
3. **Failed pushes don't write history**: `recordEncoraPush` is only
   reached on the success branch of each switch arm
   (lines 75-78, 91-95). The negative case is explicitly tested by
   `TestAPIApplySurfacesEncoraError` which asserts `events == empty`
   on `ErrRateLimited`.
4. **Destructive endpoints fenced off**: `EncoraWriteClient` interface
   exposes only `AddToCollection` and `UpdateCollectionFormat`
   (lines 23-28). The compile-time guard
   `var _ EncoraWriteClient = (*encora.Client)(nil)` at server.go:106
   ensures the real client conforms. `RemoveFromCollection`,
   `RemoveFromWants`, `AddToWants` exist on `*encora.Client` but are
   unreachable from apply paths. Comment at apply.go:21-22 codifies
   the intent.
5. **Page-load doesn't trigger writes**: `loadMismatches` is pure-read
   (`mismatch.go:102-154`); only the GET handlers call it. POSTs come
   in only on `/apply` and `/api/v1/apply`. Verified by
   `TestPagesMismatches`, which renders the HTML page without a stub
   client and the test passes.

`describeEncoraError` (line 118-132) is honest about what's missing:
the rate-limit branch acknowledges it can't pass through the
RetryAfter without a typed error wrapper. That's the cleanup itch
that ties back to concern #3 in the prior review.

`parseApplyForm` (line 266-324) is bespoke (no schema-router). Two
mild oddities:

- `splitApplyFormKey` is a hand-rolled parser instead of a regex —
  defensible since it's hot-path-by-virtue-of-being-the-only-path; the
  `unicode/utf8` rune iteration would handle e.g. an emoji index value
  but cobra's never going to send one.
- `sortInts` is an inlined insertion sort. Not worth pulling in
  `sort.Slice` for a list of ~20 items.

## Top concerns

Listed by severity.

1. **Listen default is `[::]:8080`, not loopback** — and now there's
   a write path. `internal/config/config.go:20` declares
   `DefaultListenAddr = "[::]:8080"`, line 150 sets it as the viper
   default for `server.listen`. Combined with viper's "default fills
   in empty string", this means `appConfig.Server.Listen` is *never*
   empty and the loopback fallback at `cmd/serve.go:88-90` is dead
   code. The README, the comment at `cmd/serve.go:21`, and the
   docstring at `internal/server/server.go:3` all say
   "loopback-only". `config.example.yaml:35` says `[::]:8080`
   too. Pre-Wave-2 this was theory; with `/apply` now able to push to
   Encora it's an exposure. Either flip the default to
   `127.0.0.1:8080` (smallest fix), or refuse to bind on a non-loopback
   address when `s.encora != nil` and an auth middleware isn't
   configured.

2. **`Retry-After` parsed but never honored** — the previous review
   flagged this; the parse half landed (`encora/client.go:284-290`,
   tested by `TestParseRateLimitRetryAfter`) but the consumer half is
   still missing. `internal/sync/sync.go` doesn't sleep on
   `rl.RetryAfter`; `internal/server/apply.go:118-132`
   acknowledges in its comment that "the encora client doesn't yet
   thread RateLimitInfo through the error itself" and surfaces a
   generic message. With the apply workflow now POSTing in a tight
   loop, an Encora 429 in the middle of a multi-action batch will
   429 every subsequent action in the batch (no backoff). Either
   wrap the sentinel as `&RateLimitedError{RetryAfter: rl.RetryAfter}`
   so callers can switch on it, or have `apply.go` short-circuit the
   loop on `errors.Is(err, ErrRateLimited)` so the user gets one error
   instead of N.

3. **Apply trusts the form's `recording_id` and `new_format` blindly**
   — `internal/server/apply.go:172-188` and the HTML form handler at
   234-258 don't validate that the supplied `recording_id` exists
   locally, that the recording is currently in the state the form
   claims (e.g. someone could send `type=add_to_collection` for a
   recording that's *already* in the collection), or that
   `new_format` matches what `loadMismatches` would currently produce.
   The chrome of the page is correct (only "real" mismatches render
   checkboxes, hidden inputs match the live computed format), but a
   cURL-from-localhost can ask the server to push arbitrary
   format strings or duplicate-collect already-collected recordings.
   Mitigation in tandem with concern #1: the localhost binding limits
   exposure today, but a `loadMismatches`-then-cross-check guard inside
   `applyOne` would close the gap.

4. **Apply uses `c.Request().Context()` for every action serially** —
   `apply.go:185, 237`. If the client cancels mid-batch (browser nav,
   `curl --max-time`), already-committed pushes have already happened
   on the upstream and the local history reflects them, but un-fired
   actions will now error on a cancelled context and not even be
   reported in the response. Consider a separate detached context
   (`context.WithoutCancel(ctx)` from Go 1.21+) or at least surfacing
   "client cancelled" as a per-action result so the user can audit
   what fired.

## Nits

- **`paramInt` uses `json.Unmarshal` for a single int** at
  `internal/server/api.go:453-466`. `strconv.Atoi` is the idiomatic
  call. Same outcome, but the JSON path will silently accept `"1.0"` →
  fail, and accept exponential `"1e2"` → 100, which is unlikely what
  the user wanted.
- **Legacy `loadRecordingsList`** is still referenced by `/api/v1/wants`
  (api.go:158) and `/wants` page handler (pages.go:223) even though
  the deprecation comment is in place. Migrate them to
  `loadStatefulRecordings` and drop the dead helper to remove ~50 LoC.
- **`scanner.runOne` always logs at Info** (`scanner.go:97-104`) even
  when nothing changed — `enqueued=0 removed=0 skipped=0`. With a
  1-minute polling default that's a log line every minute forever.
  Consider Debug when all counts are zero.
- **`fillResolvedCast` does N+1 queries** for performer + character
  per cast row (`internal/storage/recordings.go:120-133`). One batch
  IN-clause query each (mirroring `loadRecordingMeta`'s pattern in
  `internal/server/api.go:344-396`) would halve detail-page latency
  for big-cast shows.
- **`computeStatusCounts` runs a full `ListStates`** on every home-page
  load (`internal/server/pages.go:109-119`) just to bucket-count by
  status. With ~50 recordings this is fine; once it's 5000 a SQL
  GROUP BY on the same UNION query would be cheaper.
- **`scanner.confidenceFor` returns `""`** to mean "errored, drop the
  suggestion" (line 251-261). The empty-string overload is a
  pattern-mark for ConfidenceLow/High; a separate `bool ok` return
  would read clearer.
- **`sortInts`** at `apply.go:356-364` is bespoke. `sort.Ints(s)` from
  the stdlib does the same thing for free.
- **`EnqueueFile`'s post-upsert SELECT** (`import_queue.go:67-93`) is
  defensive against a SQLite quirk that doesn't actually bite — modernc
  driver's `LastInsertId` returns the rowid on UPSERT correctly. Worth
  a TODO comment with a reproducer if there ever was one.
- **`paramInt` accepts negative-rejection** but not zero-rejection
  (`api.go:462`). `?limit=0` returns the default; a more obvious choice
  is to honor zero as "no limit", consistent with storage's
  ListHistoryOptions semantics.
- **Discovery probe testdata** (`docs_html.json`, `openapi.json`,
  `performers_index.json`, etc.) is committed under
  `internal/stagemedia/testdata/` but isn't loaded by any test.
  Untracked-in-status: still present after commit `142c0fe`
  ("chore: drop accidentally-committed stagemedia discovery probes").
  These were re-added later as `?` files in `git status`. Either
  delete from disk or `.gitignore` them; today they bloat the directory.
- **`promptbook.db`** sitting in the repo root (untracked, gitignored).
  Likely a local-dev artifact from running against the example config.
  Not committed — fine, just noting.

## Verification commands run

```text
$ git -C ... log --oneline 6dc1aed..HEAD | wc -l       # 51 logical commits (62 with merges)
$ git -C ... diff 6dc1aed..HEAD --stat | tail -1       # 74 files, 9226+ insertions
$ git -C ... branch -vv | grep '* overnight'           # no upstream — local-only confirmed
$ git -C ... log 6dc1aed..HEAD --pretty=full | grep -i "co-author\|claude"  # nothing
$ task build                                            # up to date
$ rm -rf .task && task build                            # clean rebuild succeeds
$ task lint                                             # 0 issues (golangci-lint v2) + 0 markdown errors
$ task test                                             # all packages PASS
$ go test -race ./...                                   # all packages PASS, race-clean
$ go vet ./...                                          # clean
$ ./build/promptbook --help                             # noun-verb tree
$ ./build/promptbook library --help                     # ingest|nfo|queue|rename|scan|watch
$ go test -run 'TestAPIApply' -v -race ./internal/server/    # 5 apply tests PASS
$ git ls-files | grep -E '(\.db|config\.yaml)$'         # nothing — both gitignored
$ grep -rn 'RemoveFromCollection\|RemoveFromWants' --include='*.go' \
    -- ./internal/server/                               # not on EncoraWriteClient
$ grep -rn 'go func\|goroutine' internal/server/apply.go        # no concurrent paths in apply
```

`task test` summary for context: 134 test functions across 10
packages, all passing. New named tests covering this batch include
`TestSyncPopulatesPeopleTables`, `TestSyncBailsOnRateLimitFloor` (now
table-driven over remaining=0 and remaining=1),
`TestEngineIngestAutoAddsUnknownID(WithCollection)`,
`TestRecordingPageNFTWarning`, `TestAPIMismatchesEnumerates`,
`TestAPIApplyHandlesAddToCollection`, `TestAPIApplyHandlesFormatMismatch`,
`TestAPIApplyMissingFileShortCircuits`, `TestAPIApplyWithoutEncoraClient`,
`TestAPIApplySurfacesEncoraError`, `TestComputeStatus` (10-case
truth table), `TestComputeFormatString`, `TestParseRateLimitRetryAfter`.
