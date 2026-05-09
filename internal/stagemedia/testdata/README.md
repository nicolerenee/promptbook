# StageMedia API fixtures

Captured 2026-05-09 against `https://stagemedia.me/api/`. Useful for unit tests
against the `Images` and `Performer` types.

## Files

| File | Endpoint | Notes |
|------|----------|-------|
| `90100728.json` | `GET /images?show_id=90100728&actor_ids=1` | 200 success — empty posters, one performer |
| `90100728-no-actor.json` | `GET /images?show_id=90100728` | 400 "No actors" — actor_ids is required |
| `greenwich-beacon.json` | `GET /images?show_id=greenwich-beacon&actor_ids=1` | 400 "Invalid show_id" — slug not accepted, must be int |

`*.headers` files are the raw response headers.

For the full recon writeup, see `../../../docs/stagemedia-api.md`.
