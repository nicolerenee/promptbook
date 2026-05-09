# StageMedia API Recon — 2026-05-09

Probed against `https://stagemedia.me/api/`. Bearer-token auth (key in 1P at
`op://kube-shared/stagemedia-api/credential`). Reverse-engineered from
[pekempy/Jellyfin.Plugin.Encora](https://github.com/pekempy/Jellyfin.Plugin.Encora/blob/master/Jellyfin.Plugin.Encora/Providers/StageMediaMovieImageProvider.cs)
plus three live probes.

## The only endpoint we need

`GET /api/images?show_id={int}&actor_ids={int}[,{int}...]`

- **`show_id`**: numeric Encora show id (NOT slug). Required.
- **`actor_ids`**: comma-separated numeric Encora performer ids. Required, must
  contain at least one. The Jellyfin plugin hardcodes `actor_ids=1` when it
  only wants posters — `1` is treated as a sentinel since the API hard-rejects
  the call without an `actor_ids` value at all.
- Headers: `Authorization: Bearer {key}`, `Accept: application/json`. The
  plugin also sets `User-Agent: JellyfinAgent/0.1` — promptbook should set its
  own UA for the same trace-friendliness.

## Response shape

```json
{
  "posters": ["https://stagemedia.me/storage/posters/...", "..."],
  "performers": [
    {"id": 1, "url": "https://fixture.invalid/storage/headshots/01JFIXTUREHEADSHOT00000000000.jpg"}
  ],
  "error": null
}
```

- `posters` is an array of fully-qualified poster URLs. Empty array when the
  show has no curated posters.
- `performers` echoes back only the actor ids it found a headshot for; ids you
  asked about that aren't on stagemedia are silently dropped, not returned
  with `url: null`.
- `error` is `null` on success, otherwise a short string ("Invalid show_id",
  "No actors").

## Observed status codes

| Status | When |
|--------|------|
| 200 | success — `error` is null, `posters`/`performers` may be empty arrays |
| 400 | invalid `show_id` (e.g. non-numeric) or missing `actor_ids` |

## Fixtures saved

All in `internal/stagemedia/testdata/`:

| File | Status | Notes |
|------|--------|-------|
| `90100728.json` | 200 | empty posters, one performer entry — promptbook's typical response shape |
| `90100728-no-actor.json` | 400 | `show_id=90100728&actor_ids=` omitted → "No actors" |
| `greenwich-beacon.json` | 400 | `show_id=greenwich-beacon` (slug, not int) → "Invalid show_id" |

Headers are alongside under `*.headers`.

## Implications for promptbook

- One client method covers both poster lookup (sentinel `actor_ids=1`) and
  performer-batch headshot lookup (real ids).
- Posters/headshots returned are fully-qualified hot-link URLs to stagemedia's
  CDN. promptbook should download + cache them locally so detail-page loads
  don't depend on the upstream and so we survive image deletions.
- The "drop unmatched performer ids" behavior means the client should
  back-fill nil/empty entries for ids the user asked about that came back
  empty — otherwise we'd silently mistake "stagemedia doesn't have it" for
  "stagemedia returned nothing".
- No documented rate limit. Be polite (small batch sizes per call, sleep
  between batches when refreshing the whole library).
