# Encora API fixtures

Captured 2026-05-08 during Phase 0 recon, against
`https://encora.it/api/`. Useful for unit tests against the `Recording`,
`CollectionEntry`, `Page[T]`, and related types in `../types.go`.

## Files

| File | Endpoint | Notes |
|------|----------|-------|
| `profile.json` | `GET /profile` | Email field redacted before commit. |
| `collection.json` | `GET /collection` | 28 owned recordings, Laravel-style page envelope. |
| `wants.json` | `GET /wants` | 14 wanted recordings. |
| `recording_8222.json` | `GET /recording/90100222` | Marigold the Musical (Broadway, 2009 pro-shot). |
| `recording_8222_screenshots.json` | `GET /recording/90100222/screenshots` | Array of 1 URL. |
| `recording_8222_subtitles.json` | `GET /recording/90100222/subtitles` | 3 subtitle entries. |
| `masters.json` | `GET /masters` | Empty array for this user. |
| `probe_2008312.json` | `GET /recording/90100312` | Non-owned recording — confirms detail lookup works for any ID. |

`*.headers` files are the raw response headers (status line + rate-limit info).

## Notable

- `collection.data[i].recording` is byte-identical to `/recording/{id}`. Sync
  doesn't need per-record fan-out.
- `Date.full_date` is always ISO. Use `month_known`/`day_known` to format
  partial dates ("December 2009" vs "2009-12-01").
- `metadata.has_screenshots` / `has_subtitles` gate optional follow-up calls.

For the full recon writeup, see `../../../docs/encora-api.md`.
