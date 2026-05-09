# Encora API Recon — 2026-05-08

Probed with 8 requests, well under 30/min cap. Raw responses saved alongside.

## `/recording/{id}` works for any ID, not just owned ✅

Confirmed via id=90100312 (a Painted Stallions UK Tour bootleg, 4 owners worldwide,
not in our collection). Returns 200 with full cast/dates/notes/master.

Implication: renamer + NFO generator can resolve any encora ID the user supplies,
not just IDs in their collection. The plan doc's "files not in user's collection
need to be added to Encora first" caveat softens — if you can find the ID, promptbook
can fetch the data.

## Answers to plan-doc open questions

### 1. Does `/collection` have enough to skip per-record detail calls? ✅ YES

`collection.data[i].recording` is byte-identical to `/recording/{id}` (verified via diff).
So sync = one paginated call. For 28 recordings = 1 request total. Big collections page
at per_page=100 (Laravel-style: `next_page_url`, `current_page`, `last_page`, `total`).

### 2. Does recording detail bundle show-level metadata? ⚠️ PARTIAL

- ✅ `metadata.show_description` — HTML plot/synopsis
- ✅ `cast` — performers + characters + status (u/s, swing)
- ✅ `metadata.venue`, `metadata.city`, `metadata.recording_type`
- ❌ Composer/lyricist — not in response (would need hand-curation if we want it)

### 3. Screenshots shape

Plain JSON array of URL strings to encora's CDN:

```json
["https://fixture.invalid/storage/0099/marigold-junction-screenshot.png"]
```

Hot-linkable. For "custom images" requirement, we cache or override in promptbook.

### 4. Date format ✅ Better than expected

Structured object:

```json
{"full_date": "2009-12-01", "month_known": true, "day_known": false, "date_variant": null, "time": "evening|matinee|unknown"}
```

`full_date` always ISO. `day_known=false` means the day part is a placeholder — show as "December 2009" not "December 1, 2009". `date_variant` likely numeric for multi-recordings-same-date disambiguation (null in samples).

### 5. Tour name canonicalization

Looks clean ("Broadway", "First US National Tour"). Will see during sync if any drift.

## Other notable shape details

- `recording.master`: short tag — "pro-shot" for proshots, master/taper name for bootlegs
- `recording.metadata.recording_type`: "pro-shot", "audio", "video" — useful for filtering
- `recording.notes`: long-form notes (HTML/markdown allowed)
- `recording.nft`: `{nft_date, nft_forever}` — Not For Trade gating
- `recording.metadata.is_preview`, `is_opening`, `is_closing`, `is_concert`, `is_nfs` — flags
- `recording.metadata.has_screenshots`, `has_subtitles` — booleans, avoid pointless calls
- `recording.metadata.owners_count`, `wanters_count` — popularity metrics (per-recording, OK to display)
- Top-level on collection entry: `format` (user's owned format, e.g. "MKV (1080p - h.264) - 8.74 GB"), `notes` (user's personal notes), `collected_at`, `updated_at`, `user_watched`
- Wants entry adds `priority` (null in samples) and `added_at`
- Subtitles: `{recording_id, language, author, file_type, coverage, url}` per entry

## Rate limit

Headers per response:

- `x-ratelimit-limit: 30`
- `x-ratelimit-remaining: N`
- (No Retry-After observed; would appear on 429)

Behavior across 7 requests in ~5s suggests a per-minute window. Sync should:

- Sleep between paginated calls
- Honor `Retry-After` on 429
- Bail-out preserve last-good if remaining hits 0 (resume next run)

## What this means for promptbook design

- **Sync**: trivial. One paginated `/collection` call + one `/wants` call gets everything for ~98% of cases. No per-record fan-out needed for v1.
- **Per-recording extras**: only fetch `/screenshots` and `/subtitles` when `metadata.has_screenshots`/`has_subtitles` is true. Probably batch in a "details" pass once, then cache forever.
- **DB schema**: model encora's date object directly (full_date + month_known + day_known) rather than collapsing into a date string.
- **Renamer**: format the date based on month_known/day_known flags. `day_known=false` → "December 2009"; both true → "2009-12-01"; year-only (`month_known=false`) → "2009".
