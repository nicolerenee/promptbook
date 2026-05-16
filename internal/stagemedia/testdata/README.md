# StageMedia API fixtures

Synthetic JSON fixtures shaped like real StageMedia responses. Used to unit
test the `Images` and `Performer` types without referencing any real show.

## Files

| File | Endpoint analog | Notes |
|------|-----------------|-------|
| `fixture-show.json` | `GET /images?show_id={n}&actor_ids=1` | 200 success — empty posters, one performer. |
| `fixture-show-no-actor.json` | `GET /images?show_id={n}` | 400 "No actors" — actor_ids is required. |
| `fixture-show-invalid.json` | `GET /images?show_id={slug}&actor_ids=1` | 400 "Invalid show_id" — slug not accepted, must be int. |
