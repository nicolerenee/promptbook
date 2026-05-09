# Promptbook Catalog + Performances Library Cleanup — Plan

Written 2026-05-07. Paused waiting on Encora API access (support ticket open).

## What this is

Three connected workstreams kicked off after the new Atlantis Jellyfin
deploy revealed that the `/store01/Performances/` tree (Broadway pro shots
and bootleg recordings) is half-organized, half-chaos, and not well-served
by Radarr/TMDB/Jellyfin's normal flow.

1. **Library cleanup** — rename messy bootleg folders/files into a canonical
   scheme that bakes the Encora recording ID into the name (mirroring how
   Radarr bakes `[tmdbid-...]` into movie folders), so future tooling has a
   reliable join key. Includes pulling unorganized files out of `~/downloads/`
   and `~/downloads/mega/` into `/store01/Performances/`.
2. **promptbook.freckle.media** — a small website at that hostname showing the
   Encora collection (owned recordings) + wants list, with show/tour drilldown
   matching how Encora itself organizes things, plus flat library/wants
   tables. For browsing with the user's daughter to pick what to watch.
3. **NFO generator** — produce Jellyfin-compatible `movie.nfo` files for
   each bootleg so Jellyfin reads metadata locally without bouncing through
   TMDB (which doesn't have most of these recordings).

The thread tying all three together: **promptbook serves a read-only mirror of
the Encora API**. Scripts (renamer, NFO generator) take an `ENCORA_BASE_URL`
env var that defaults to `https://encora.it` but flips to promptbook once it's
up. That gives outage tolerance and a place for hand-authored records
that aren't in Encora.

## Status — what's done, what's blocked

### Done

- `scripts/library-audit.py` — Radarr↔Jellyfin diff, finds movies Radarr
  has files for that Jellyfin doesn't see (and vice versa). Already used
  to chase down the November Christmas "release group named extra" gotcha
  and confirm the Performances tree as the main remaining gap.
- `scripts/encora-inventory.py` (Phase 1) — walks a Performances tree and
  parses each folder into `{show, tour, date, master, parse_confidence}`.
  Pure local, no auth. Skips Radarr-managed `[tmdbid-...]` proshots by
  default; includes them with `--include-tmdb`. Reads optional
  `encora-id.txt` sidecar to override matching.
- Memory updates: `feedback_arr_stack_tricks.md` got the "release group
  named 'extra' breaks Jellyfin" entry; `project_namespace_split.md`
  was extended to add `media-tools` as a fifth namespace.

### Blocked on Encora API key

Everything else. Encora's API requires a Bearer token issued after a
support-ticket review. Ticket is open as of 2026-05-07. Could land in
hours, could be weeks.

## Folder/file naming convention (decided)

For bootlegs, mirror Radarr's `[tmdbid-N]` pattern with `[encora-N]`:

**Folder**: `{Show} - {Tour} - {Date} [encora-{ID}]`
**File**:   `{Show} - {Tour} - {Date} [{Master}].{ext}`

Examples:

```
Tideline Manor - First US National Tour - 2024-01-21 [encora-90118317]/
  Tideline Manor - First US National Tour - 2024-01-21 [Standard Master].mp4

Mockingbird Lane - Second US National Tour (Non-Equity) - 2023-10-26 [encora-NNNNNN]/
  Mockingbird Lane - Second US National Tour (Non-Equity) - 2023-10-26 [Standard Master].mp4

Halcyon Crossing - Broadway - 2024-02-09 [encora-NNNNNN]/
  Halcyon Crossing - Broadway - 2024-02-09 [fixturetaper].mp4

Company - Broadway Revival - 2006 [encora-NNNNNN]/
  Company - Broadway Revival - 2006 [Unknown].mp4
```

Decisions:

- **Date as bare ISO with `-` separator**, not `(YYYY-MM-DD)`, so tours that
  contain parens like "Second US National Tour (Non-Equity)" don't produce
  double-paren ugliness.
- **`-` between show/tour/date** matches how it titles in Plex.
- **Master in the filename, not folder** — the encora ID already uniquely
  identifies the recording at the folder level.
- **Year-only fallback** for older recordings without precise dates
  (`Company - Broadway Revival - 2006`).

Pro shots already managed by Radarr (`[tmdbid-...]`) keep their existing
naming; we don't touch them.

## promptbook.freckle.media — app spec

### Repo layout

```
kubernetes/apps/media-tools/promptbook/
  helmrelease.yaml          # app-template chart: Deployment + CronJob
  externalsecret.yaml       # ENCORA_API_KEY from 1Password
  pvc.yaml                  # fresh ~5GB rook-ceph
  kustomization.yaml
kubernetes/clusters/atlantis-k8s01/namespaces/media-tools.yaml
kubernetes/clusters/atlantis-k8s01/apps/media-tools/
  promptbook.yaml
  kustomization.yaml
```

`media-tools` is a new namespace per the updated namespace-split plan
(`media-tools/` parallels `download-tools/`: user-facing helpers around
the corresponding "do the work" namespace). Future tenants: jellyseerr,
gethomepage.dev, anything similar.

### Containers

- **Deployment** — nginx, mounts the PVC read-only at `/usr/share/nginx/html`,
  serves both the human pages and `/api/*`. Pod restart never re-hits
  Encora.
- **CronJob (daily)** — Python script. Pulls `/api/collection`, `/api/wants`,
  per-recording `/api/recording/{id}` (rate-limited to 30/min per Encora's
  guidance, with `Retry-After` honoring), `/api/recording/{id}/screenshots`.
  Writes static HTML + JSON to the PVC.

### API mirror

The PVC layout serves `/api/*` directly:

```
/data/api/collection.json
/data/api/wants.json
/data/api/profile.json
/data/api/recording/{id}.json
/data/api/recording/{id}/screenshots.json
/data/api/recording/{id}/subtitles.json
```

nginx rewrite for path-without-extension:

```nginx
location ~ ^/api/recording/(\d+)$ {
    try_files /api/recording/$1.json =404;
}
```

Auth: the OIDC SecurityPolicy on the gateway is the auth layer for the
whole site. Bearer tokens in API requests are ignored or just
present-checked — anyone past the gateway is trusted.

### Sync semantics

- **Transient failures** (network error, 5xx, rate-limit, timeout): preserve
  the previous JSON for that record. Don't write blank/error markers.
- **Hard 404 from Encora** (record explicitly deleted): flag the local record
  as orphaned. Don't delete the local JSON. Surface in the UI and in
  `/data/api/sync/orphaned.json`. This is the case where any script
  using that ID will silently produce stale data, so it needs visibility.
- **last_synced** timestamp on every record so staleness is visible.

### Sync log

```
/data/sync-log/2026-05-07T12-34-56Z.json    # one file per run
/data/api/sync/latest.json                  # most recent run
/data/api/sync/orphaned.json                # rolling list of 404'd IDs
```

Each run records: started_at, finished_at, counts (updated / unchanged /
errored / orphaned), per-error detail, rate-limit headers. Keep last 30
runs.

### UI structure

Mirrors Encora's own three-level navigation:

```
/                           → Shows grid (combined: anything in collection or wants)
/shows/{slug}               → Show detail: tours/productions, owned + wanted
/shows/{slug}/{tour}        → Tour detail: recordings list (date, master, cast preview)
/shows/{slug}/{tour}/{date} → Single recording: full cast/roles, notes, master
/library                    → Flat table of owned recordings (sortable)
/wants                      → Flat table of wanted recordings (sortable)
/status                     → Sync log: latest run, last 30 runs, orphaned IDs
```

Auth: SecurityPolicy OIDC via freckle.id on the public Envoy gateway.
Hostname `promptbook.freckle.media`. Same gateway/auth pattern as
jellyseerr/qui.

### Tech stack

- Python + Jinja2 templates → static HTML
- Tiny vanilla JS for sortable/filterable tables on `/library` and `/wants`
- No SPA, no build step, no framework

### Show-level metadata (composer, plot, character list)

TBD on whether `/api/recording/{id}` bundles this. Plan A: it does, we use
it. Plan B (fallback): hand-curated `shows.yaml` in the repo with
composer/plot for the ~20 shows the user owns recordings of. Skip
character lists if not in API.

### Poster art

Fallback chain (script picks highest-quality available):

1. Curated Plex collection posters (one-time extract — deferred for now)
2. JPGs already in the bootleg folder (some downloads come with screenshots)
3. Encora `/api/recording/{id}/screenshots` (free poster material)
4. Default placeholder

V1 ships with chain steps 3 and 4 only. Plex extractor and in-folder JPG
support are future iterations.

## Renamer script — `scripts/encora-renamer.py`

Walks source directories, matches files against the Encora collection,
proposes canonical names, applies on confirm.

### Algorithm

1. **Cache encora collection** — fetch `/api/collection` (one call, all
   user's owned recordings), save as `encora-collection.json`. Once-per-day
   max per Encora's rate-limit guidance.
2. **Walk source dirs** — `/store01/Performances/` plus
   `/store01/downloads/` (top-level files) and `/store01/downloads/mega/`.
   For each video file or folder-with-video, parse best-effort fields
   (using the same parser as `encora-inventory.py`).
3. **Match locally-parsed → encora records** — score on show name (token
   set), date proximity (exact = 100, same month = 50, same year = 10),
   master exact match (bonus). Best score wins; below threshold = unmatched.
4. **Output** — `match-plan.json`: `local_path → encora_id (confidence) →
   proposed_canonical_path`.
5. **Print plan** — readable table of what would move where, what's
   unmatched. Default dry-run.
6. **Apply** — `--apply` executes moves into
   `/store01/Performances/{Category}/{Canonical Folder}/{Canonical File}`.

### Special cases the renamer must handle

- **Duplicates with already-organized files**: `mega/Tideline Manor 2024-1-21 M.mp4`
  is the same file as `Performances/Broadway/Tideline Manor [...] (2024-01-21) [...]`.
  Detect by size + name match, propose **delete the duplicate**, never
  double-organize.
- **Inline metadata in filenames**: `Quill Theatre Revival ... NFT - fixturetaper55.mp4`
  encodes trading status and master. We get both from Encora; drop them
  in favor of the canonical scheme.
- **`(Preview)` markers** like `The Outsiders - Mar 19 2024(Preview).mp4`
  — preview performances are an Encora distinction; pull from the
  recording detail, encode in the new filename if Encora flags it.
- **Initialism map**: `HPCC` = "Harry Potter and the Cursed Child", etc.
  Seed a small alias dict in the script; user adds more as encountered.
- **Files NOT in encora collection**: API has no public search endpoint,
  only "fetch by ID" and "list MY collection". So files not in collection
  go to `/store01/Performances/_unmatched/` and the user either adds them
  to Encora and re-runs, or hand-fills an `encora-id.txt` sidecar.

### Auth

Reads `ENCORA_API_KEY` from env. Reads `ENCORA_BASE_URL` (default
`https://encora.it`); once promptbook is deployed, point at
`https://promptbook.freckle.media` so the renamer uses the local mirror.

## NFO generator — design

The original ask. Produces Jellyfin-compatible `movie.nfo` files next to
each video file so Jellyfin reads metadata locally without TMDB.

NFO shape:

```xml
<movie>
  <title>Velvet Antlers - First US National Tour - 2024-04-14</title>
  <originaltitle>Velvet Antlers</originaltitle>
  <year>2024</year>
  <premiered>2024-04-14</premiered>
  <plot>Lead debut performance. Incredible video of this incredible cast...</plot>
  <tag>First US National Tour</tag>
  <tag>Standard Master</tag>
  <tag>Bootleg</tag>
  <set><name>Velvet Antlers</name></set>      <!-- Jellyfin auto-creates collection -->
  <studio>Disney Theatrical</studio>
  <genre>Musical</genre>
  <genre>Stage Recording</genre>
  <actor>
    <name>Natalie Goodin</name>
    <role>U/s Elsa</role>
    <order>0</order>
  </actor>
  ...
  <uniqueid type="encora" default="true">{ID}</uniqueid>
</movie>
```

Reads from promptbook API (`ENCORA_BASE_URL` env var). One pass over the
canonically-named Performances tree:

1. Regex `[encora-(\d+)]` from each folder name.
2. `GET {ENCORA_BASE_URL}/api/recording/{id}` — full detail with cast.
3. Template into `movie.nfo`, write next to the video.
4. Optionally fetch screenshots for `poster.jpg`/`fanart.jpg`.

The `<set><name>` element makes Jellyfin auto-create the collection
("Velvet Antlers", "Halcyon Crossing") that groups all recordings of the same show.
Mirrors the Plex collection grouping the user already had.

## Encora API quick reference

Discovered from <https://encora.it/api-docs> on 2026-05-07. Bearer auth.
Rate limit 30/min default. All endpoints prefix `https://encora.it/api/`.

| Method | Path | Use |
|--------|------|-----|
| GET | `/profile` | Sanity check / user info |
| GET | `/collection` | Owned recordings (THE join key) |
| GET | `/wants` | Wants list |
| GET | `/masters` | List of recorder/master names user has |
| GET | `/recording/{id}` | Full detail: cast, master, tour, date, venue, notes |
| GET | `/recording/{id}/screenshots` | Screenshot URLs (poster material) |
| GET | `/recording/{id}/subtitles` | Subtitle assets |
| GET | `/subtitles/{ids}` | Bulk subtitle fetch (comma-separated IDs) |
| GET | `/collection/assets` | Bulk screenshots+subtitles for whole collection |
| POST | `/collection/{id}/collect` | Add to collection (write — not used here) |
| POST | `/collection/{id}/remove` | Remove from collection (write — not used here) |
| POST | `/collection/{id}/format/{format}` | Update format (write — not used here) |
| POST | `/collection/{id}/notes/{notes}` | Update notes (write — not used here) |
| POST | `/wants/{id}/add` | Add to wants (write — not used here) |
| POST | `/wants/{id}/remove` | Remove from wants (write — not used here) |
| POST | `/wants/{id}/priority/{p}` | Update priority (write — not used here) |

**Notable absence**: no public search endpoint. Can only fetch by ID or
list MY collection. That's why files not in the user's collection need
either to be added to Encora first, or have their ID hand-filled via
sidecar.

**Privacy stance from Encora docs**: endpoints for viewing other users'
profiles or collections, or accessing recording owners/wanters, will
NOT be added. So we can't enrich with "44 people own this recording"
counts at scale; that data only appears on website pages.

**Rate-limit headers** to honor:
- `X-RateLimit-Limit` (per-minute total)
- `X-RateLimit-Remaining`
- `X-RateLimit-Reset` (epoch when limit resets)
- `Retry-After` (seconds — honor this on 429)

## Open questions to resolve when API key arrives

These get answered with a few minutes of curl probing once the key is in
hand:

1. **Does `/api/collection` return enough fields per record** (show name,
   tour, date, master) **to do matching without per-record detail calls?**
   If yes, the renamer's auto-match works off one API call. If no, we
   batch detail calls and pace against the rate limit.
2. **Does `/api/recording/{id}` bundle show-level metadata** (composer,
   lyricist, plot, character list)? If yes, promptbook show pages get rich
   automatically. If no, fall back to hand-curated `shows.yaml`.
3. **What's the response shape for `/api/recording/{id}/screenshots`?**
   URLs to encora's CDN, or base64, or paths we'd need to fetch through
   another endpoint? Determines whether we hot-link or download-and-cache.
4. **Date format consistency**: does Encora return ISO `2024-01-21`,
   month-only `January, 2024`, freeform? Affects renamer's date-matching
   tolerance.
5. **Tour name canonicalization**: does the API return "First US National
   Tour" identically across all recordings of that tour, or are there
   variants ("1st US National Tour", "First US Tour")? Affects the
   renamer's grouping.

## When picking this back up

Resume order:

1. Confirm API key is active. `curl -H "Authorization: Bearer $KEY"
   https://encora.it/api/profile` returns user info.
2. Probe `/api/collection`, `/api/recording/{id}`, `/api/recording/{id}/screenshots`
   against one or two known recordings. Save responses to
   `zz-scratchdir/encora-samples/` for reference. Resolves the open
   questions above.
3. Build the promptbook Python generator script (it's the load-bearing piece —
   produces both the user-facing site AND the local API mirror that
   subsequent scripts depend on). Iterate locally until output is clean.
4. Stand up the Helm release / Flux Kustomization / namespace / route /
   OIDC SecurityPolicy. Should mirror the patterns in
   `kubernetes/apps/media-servers/jellyfin/` and the existing OIDC-protected
   apps for shape.
5. Once promptbook is serving its API mirror, build `scripts/encora-renamer.py`
   pointed at promptbook (no live encora hits during a rename run). Dry-run
   first, verify, then `--apply` on Performances tree + downloads.
6. After folders are renamed, build the NFO generator. Walks the
   canonically-named tree, regexes encora IDs, fetches detail from promptbook,
   writes NFOs.
7. Add the new Jellyfin library at `/store01/Performances/`. Watch it
   pick up the bootlegs cleanly via NFO metadata.

## Pointers to memory entries with related context

- `project_namespace_split.md` — fifth namespace `media-tools` planned for
  user-facing helper apps (promptbook, seerr, homepage). Genuine tension noted
  about "media tools" being colloquially broader than this namespace.
- `feedback_arr_stack_tricks.md` — release-group-named-"extra" gotcha,
  library-scope vs. matching-failure rule of thumb, path-mismatches-during-
  upgrade behavior.
- `project_media_strategy.md` — Jellyfin primary, Plex fallback, Apple TV
  via Infuse. Frames why Performances belongs as a Jellyfin library.
- `project_envoy_gateway_migration.md` — how OIDC SecurityPolicy is wired
  for new public-gateway apps. promptbook will follow the same pattern.
