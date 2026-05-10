# Media-server refresh hooks (planned)

Status: design doc only — not implemented yet.

## Problem

Promptbook generates NFO files alongside each recording, including image
URLs that point back at promptbook's `/images/*` routes. We rewrite the
NFO whenever an image changes (the `nforefresh.Service` cascade landed
in commit `6b1245b`):

- Recording poster / fanart change → rewrite that recording's NFO.
- Show banner change → rewrite every NFO under that show.
- Performer headshot change → rewrite every NFO featuring the performer.

We also cache-bust the image URLs with `?v={unix-mtime}` (or
`?v=generated` for placeholders), so the URL itself changes when the
underlying image does.

Despite all that, **Jellyfin doesn't pick up image changes on its own**:

1. **HTTP-layer cache stickiness.** Even with `?v=` cache-busting,
   Jellyfin's internal image storage doesn't aggressively re-check
   URLs once an item has a stored image. The new URL gets read into
   the NFO on the next scan, but Jellyfin's existing image binary
   stays put.
2. **Global people pool.** Per-recording `<actor><thumb>` URLs are
   extra-bad. Jellyfin maintains a single global metadata directory
   for performers (`<jellyfin-config>/metadata/People/...`). Once it
   has a face for a given performer, it serves that across every
   recording regardless of what each NFO says about that performer's
   thumb URL.

The user-facing symptom: re-uploading a headshot via the picker has no
visible effect in Jellyfin until the user manually triggers
"Refresh metadata → Replace existing images," and even that often
doesn't touch the people pool. Plex behaves similarly (no real-time
NFO watcher; relies on manual or scheduled refreshes).

## Goal

When promptbook rewrites an NFO, also tell the configured media
server(s) to refresh that specific item — including images — so the
new URLs actually get re-fetched.

Out of scope: changing how promptbook generates URLs or NFOs. The
existing cascade is correct; the missing piece is the trigger to the
downstream media server after each rewrite.

## Design

### Config

New config block, both servers optional:

```yaml
mediaServers:
  jellyfin:
    url: "https://jellyfin.example.com"
    apiKey: ""              # PROMPTBOOK_MEDIASERVERS_JELLYFIN_APIKEY
    # libraryID is optional — when set the per-item lookup scopes
    # to that library. When empty we search globally.
    libraryID: ""
  plex:
    url: "https://plex.example.com:32400"
    token: ""               # PROMPTBOOK_MEDIASERVERS_PLEX_TOKEN
    libraryID: ""           # Plex requires a section id for path-based refresh
```

Both blocks empty → no refresh hook fires; promptbook behaves exactly
as it does today.

### New package: `internal/mediaserver`

```go
type Refresher interface {
    // RefreshRecording forces a per-item metadata + image refresh
    // for the recording with the given encora id. No-op when the
    // backing client isn't configured. Errors are logged but not
    // surfaced to callers — refresh is best-effort.
    RefreshRecording(ctx context.Context, encoraID int64)
}

type Service struct {
    backends []Refresher  // jellyfin, plex, ... — call all configured
    logger   zerolog.Logger
}

func (s *Service) RefreshRecording(ctx context.Context, encoraID int64) {
    for _, b := range s.backends {
        b.RefreshRecording(ctx, encoraID)
    }
}
```

Two backends today:

- `JellyfinRefresher` (config: url, apiKey, optional libraryID)
- `PlexRefresher` (config: url, token, libraryID)

More backends slot in via the same interface.

### Jellyfin backend

Two-step API call per refresh:

1. **Find the item id.** Jellyfin indexes our `<uniqueid type="encora">`
   value as a provider id. The lookup endpoint:

   ```
   GET {url}/Items
       ?providerIds=Encora.{id}
       &Recursive=true
       &Fields=Path
       &api_key={apiKey}
   ```

   Returns `{ Items: [{ Id, Path, Name }, ...] }`. Take the first
   match. (Should usually be exactly one.)

2. **Trigger the refresh** with full replace flags:

   ```
   POST {url}/Items/{itemId}/Refresh
       ?MetadataRefreshMode=FullRefresh
       &ImageRefreshMode=FullRefresh
       &ReplaceAllMetadata=true
       &ReplaceAllImages=true
       &api_key={apiKey}
   ```

   Returns 204 on success. The refresh runs async on Jellyfin; we
   don't wait for it.

When the lookup fails (item not yet imported, encora id not indexed),
log at debug and move on. Don't error — most likely cause is the user
hasn't scanned their library yet.

### Plex backend

Plex's NFO agent (XBMC NFO Movies Import) refreshes via either:

- **Per-item refresh** if we know the rating-key — needs a path lookup
  via `GET /library/sections/{sectionId}/all?title={...}` then a
  `PUT /library/metadata/{ratingKey}/refresh`.
- **Path-scoped refresh** — simpler, doesn't require knowing the
  rating-key:

  ```
  POST {url}/library/sections/{sectionId}/refresh
       ?path={url-encoded folder}
       &X-Plex-Token={token}
  ```

  Refreshes only the items under that path. Folder = the recording's
  destination folder (the dirname of `recording_versions[0].file_path`).

Use the path-scoped variant — simpler, no rating-key lookup needed.

Same fail-silently-and-log policy as the Jellyfin backend.

### Wiring into the cascade

`nforefresh.Service` already has the three rewrite methods:

- `RewriteForRecording(ctx, recordingID)` → refresh that recording.
- `RewriteForShow(ctx, showID)` → refresh every recording in the show.
- `RewriteForPerformer(ctx, performerID)` → refresh every recording
  featuring the performer (largest fan-out — popular performers can
  hit 50+ recordings).

After each successful per-recording rewrite, fire
`mediaserver.Service.RefreshRecording(ctx, recordingID)`. The
mediaserver call is non-blocking on the user's upload response (already
true for the rewrite cascade — it runs as a fire-and-forget goroutine).

### Performer fan-out caveat

A performer headshot change triggers a refresh on every recording
they're in. If the performer is in 100 recordings, that's 100 Jellyfin
API calls. Per call:

- 1 lookup (`GET /Items?providerIds=...`)
- 1 refresh trigger (`POST /Items/{id}/Refresh`)

Jellyfin's API isn't rate-limited by default, but a tight loop of 200
HTTP calls in a goroutine is rude. Throttle: a small worker pool (4–8
concurrent) with a token bucket would be polite. Worth implementing
even though Jellyfin will probably handle the burst fine.

## Test plan

- `internal/mediaserver/jellyfin_test.go` — table-driven tests against
  an `httptest.Server` that scripts the lookup + refresh responses.
  Verify the refresh URL has the four `*Refresh*` query params + the
  api_key.
- `internal/mediaserver/plex_test.go` — similar shape, scripts the
  path-scoped refresh response.
- `internal/mediaserver/service_test.go` — assert all configured
  backends fire on `RefreshRecording`; an empty-config Service is a
  no-op.
- Hook-into-cascade test (in `internal/nforefresh/`): assert the new
  `mediaserver.Service` is called once per recording after each
  rewrite method runs. Use a stub `mediaserver.Refresher`.

## Open questions

1. **Should the cascade ALSO call mediaserver when promptbook initially
   ingests a recording (not just on later image changes)?** Probably
   yes — same code path. Jellyfin's auto-scan catches new files but
   the manual trigger is faster + reliable.
2. **Library ID on Jellyfin** — do we need to scope the lookup, or is
   global recursive search good enough? Global is simpler; only worth
   scoping if the user's Jellyfin has multiple libraries with
   colliding encora ids (unlikely).
3. **Token rotation** — viper reloads config on signal. Should the
   mediaserver clients re-read their tokens on rotation? Probably yes
   eventually; not load-bearing for v1.

## Suggested commit shape when implemented

One commit. Roughly:

```
feat(mediaserver): refresh Jellyfin/Plex on NFO change

Wires a mediaserver.Service into nforefresh.Service so each NFO
rewrite triggers a per-item refresh on the configured downstream
media server. Bypasses Jellyfin's sticky image cache + global
people-metadata pool by passing ReplaceAllMetadata + ReplaceAllImages
on every refresh; bypasses Plex's "scan only when explicitly told"
default by hitting the path-scoped section refresh after each
rewrite.

Configured via two new optional config blocks (mediaServers.jellyfin,
mediaServers.plex). Empty config preserves today's behaviour exactly.
Performer-headshot fan-out throttles to 8 concurrent refreshes via a
worker pool to avoid hammering the downstream server.
```

Estimated effort: agent task, ~30 min. Includes config plumbing,
two backend implementations, throttle pool, tests, and the cascade
hook. Worktree-isolated so it doesn't block other work.
