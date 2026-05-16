# Encora API fixtures

Synthetic JSON fixtures shaped like real Encora responses. Used to unit-test
the `Recording`, `CollectionEntry`, `Page[T]`, and related types in
`../types.go` without referencing any real recording, master, performer, or
storage URL.

All IDs live in a clearly-synthetic 90_000_xxx+ range so they cannot collide
with real Encora rows. Show / cast / venue names are invented. Storage URLs
point at `fixture.invalid`.

If you need to change any of these, also update the corresponding Go test
assertions in `internal/encora/client_test.go`, `internal/sync/sync_test.go`,
and `internal/nfo/writer_test.go`.

## Files

| File | Endpoint analog | Notes |
|------|-----------------|-------|
| `profile.json` | `GET /profile` | Synthetic user `fixturearchive`. |
| `collection.json` | `GET /collection` | 28 fictional owned recordings, Laravel-style page envelope. |
| `wants.json` | `GET /wants` | 14 fictional wanted recordings. |
| `recording_pilot.json` | `GET /recording/{pilot}` | The rich fixture: detailed cast / metadata for round-trip + NFO tests. |
| `recording_pilot_screenshots.json` | `GET /recording/{pilot}/screenshots` | Array of 1 URL. |
| `recording_pilot_subtitles.json` | `GET /recording/{pilot}/subtitles` | 3 subtitle entries. |
| `masters.json` | `GET /masters` | Empty array. |
| `probe_unowned.json` | `GET /recording/{other}` | A recording not in the synthetic collection, to confirm detail lookup works for any ID. |

## Notable

- `collection.data[i].recording` is byte-identical in shape to the standalone
  `/recording/{id}` payload, so sync does not need a per-record fan-out call.
- `Date.full_date` is always ISO. Use `month_known` / `day_known` to format
  partial dates.
- `metadata.has_screenshots` / `has_subtitles` gate optional follow-up calls.

For the (historical) recon writeup of the real API shape, see
`../../../docs/encora-api.md`.
