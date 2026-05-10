# Multipart files + extras (planned)

Status: design doc only — not implemented yet.

## Problem

Today's import flow assumes one folder = one main file = one recording.
The folder-as-unit scanner (commit `ed550a6`) recognizes that folders
can hold extra media beside the main file and reports an `extras_count`
on the queue row, but the import itself moves only the main file and
leaves everything else in the source folder. Three real-world cases
this breaks down on:

1. **Multipart recordings** — `act-1.mkv` + `act-2.mkv` in one folder
   are *one recording* split across two files. The matcher already
   detects act / part / pt markers (`PartIndex`, `PartKind` in
   `internal/match/parser.go`). The rename engine has a `{Part}` token
   wired into the new default file template (`fd84031`). But the
   import path picks the larger file as "main" and treats the other as
   an extra — wrong: both files are the recording.
2. **True extras alongside the main file** — Halcyon Crossing drop with main
   `.mkv` + `audio/` subdirectory containing per-track rips + a photo
   directory. The audio rips and photos are content the user wants
   preserved, organised, and visible — but not as separate recordings.
3. **Ambiguous bonus content** — Greenwich Beacon drop with main `.mkv` +
   `bows! baker, burrage.mp4` (the curtain call from a notable cast
   member's last show). The bows file isn't a sibling recording; it's
   a featurette tied to the main recording. The folder-as-unit
   heuristic today would pick the larger main as the primary and treat
   `bows!.mp4` as an unstructured extra; we'd lose the semantic that
   it's a featurette.

We need:

- Multipart imports that produce **one recording** with **multiple
  parts** of a single version.
- Extras imports that move content alongside the recording with
  semantic kinds Jellyfin understands.
- A picker UI that surfaces ambiguous cases (multiple videos, no
  part markers) so the user assigns roles before commit.

## Goal

After this work lands:

- A folder with `act-1.mkv` + `act-2.mkv` imports as one recording
  with two part rows under the same version. The rename engine emits
  `... - part-1.mkv` and `... - part-2.mkv` filenames; Jellyfin auto-
  joins them at playback.
- A folder with main + `audio/` subfolder + photos imports the main as
  the recording; the audio folder + photos move into the canonical
  destination as labelled extras.
- A folder with main + `bows!.mp4` opens the import modal showing both
  files; the user assigns `bows!.mp4` the **Featurette** role; both
  files move into the canonical layout.

## Data model

### `recording_versions` — add `part_index`

```sql
ALTER TABLE recording_versions ADD COLUMN part_index INTEGER DEFAULT 0;
```

- `part_index = 0` (default) — single-file version, today's shape.
- `part_index >= 1` — one of N parts comprising a single version. Two
  rows with the same `recording_id` and consecutive `part_index` form
  one multipart version. The unique index becomes
  `(recording_id, file_path)` (already true) plus the implicit
  `(recording_id, part_index)` covenant when both are non-zero.

The existing `recording_versions.format_label`, `quality`,
`video_codec` etc. fields still live on the parent row of part 1; the
later parts inherit display values from part 1 (or just render their
own probe data — both parts get probed independently so the values
should agree).

### `recording_extras` — new table

```go
type RecordingExtra struct {
    ID            int64
    RecordingID   int64
    FilePath      string   // canonical path under the recording's folder
    Kind          string   // featurette | scene | behindthescenes | trailer | deleted | interview | other | photo | audio
    Label         string   // user-visible label, e.g. "Bows — Baker / Marinerage"
    FileSizeBytes int64
    AddedAt       time.Time
}
```

Backed by a new ent schema + a Goose migration. Indexed on
`recording_id`.

`Kind` values match Jellyfin's extras subfolder conventions
(`featurettes/`, `scenes/`, `behindthescenes/`, `interviews/`,
`shorts/`, `trailers/`, `deletedscenes/`, `other/`) plus two
non-Jellyfin kinds promptbook tracks anyway: `audio` (per-track audio
rips, no Jellyfin equivalent) and `photo` (production photos, also no
Jellyfin equivalent — they live alongside but Jellyfin won't surface
them).

The existing `Recording.extras` GraphQL field that today walks
`source_folder` at read time gets replaced by this persisted source.
The walking-extras-at-read-time pattern from phase 2 was a v0; the
typed table is the durable shape.

## Scanner changes

`internal/scanner/scanner.go`. Today's `walkFolderUnit` →
`collectMediaFiles` → `mainFile` heuristic picks one main file.
Replace the single `mainFile` choice with a richer
`folderClassification`:

```go
type FolderClassification struct {
    // Parts is the ordered list of files that together make up the
    // recording. Singleton for non-multipart drops, multi-element
    // for act-1/act-2/pt-1/pt-2 cases.
    Parts []ClassifiedFile
    // Extras is every other media file in the folder tree. Each
    // carries a heuristic-suggested kind that the import modal
    // surfaces for user override.
    Extras []ClassifiedFile
    // Ambiguous, when true, signals the heuristic couldn't pick a
    // confident main file (multiple similarly-sized videos at root,
    // no part markers). Forces the modal to prompt before commit.
    Ambiguous bool
}

type ClassifiedFile struct {
    Path        string
    SizeBytes   int64
    SuggestedKind string  // "main" | "part-1" | "part-2" | "extra-{kind}"
    PartIndex   int       // 0 for non-parts, >0 for parts
}
```

Classification rules:

1. **Part markers** in filename (`act 1`, `act-2`, `pt 1`, `part 2`):
   group those files as `Parts` ordered by index. Other media files
   become `Extras`.
2. **No part markers**, single video file at root: that's the main;
   anything else is an extra.
3. **No part markers**, multiple videos at root (within 10% size):
   `Ambiguous = true`; suggest the largest as `main` and the rest as
   `extra-other`. Modal prompts.
4. **Subdirectory media** (audio/, photos/, behindthescenes/):
   everything inside is an extra; the subfolder name maps to the
   extras `Kind` (audio → audio, behindthescenes → behindthescenes,
   etc.). Photos folder maps to `photo`.
5. **Top-level extras heuristics** for video files: filename
   substrings drive the suggested kind:
   - `bows`, `curtain`, `applause` → `featurette`
   - `behind the scenes`, `bts`, `rehearsal` → `behindthescenes`
   - `interview` → `interview`
   - `trailer`, `commercial`, `promo` → `trailer`
   - everything else → `other`

The classification result rides on the queue entry. Add a JSON
column `classification_json` on `manual_import_queue` so the modal
sees the full set of files + the heuristic-suggested kinds without
re-walking the folder.

## Queue import modal — multi-file picker

`internal/web/static/app/components/QueueImportModal.js` (current
modal carries one file). New section between the match picker and the
filename preview:

> **Files in this folder**
>
> | File | Kind | Size |
> |---|---|---|
> | Greenwich Beacon - 2NT - 2022.07.24 M.mkv | [Main ▼] | 8.5 GB |
> | bows! baker, burrage.mp4 | [Featurette ▼] | 412 MB |

Kind dropdown options: Main / Part 1 / Part 2 / Part 3 / Featurette /
Scene / Behind the scenes / Interview / Trailer / Deleted / Other /
Photo / Audio / **Skip** (don't import).

Defaults populate from the heuristic-suggested kinds. The user can
override any row.

Modal validation:

- Exactly one row must be Main, OR ≥2 rows must be Part-N (multipart).
- At most one row per Part-N value.
- Skipping is always allowed.

When the heuristic produced `Ambiguous = true`, the modal opens with
the file list **expanded by default**. Otherwise it stays collapsed
behind a "Show file list" disclosure — single-file folders shouldn't
gain UX friction.

The destination preview ("DESTINATION" section) re-runs whenever the
user changes a kind. For multipart cases the preview shows both
canonical destination paths.

The mutation surface today is `importQueueEntry(input: { queueID,
recordingID })`. Extend it:

```graphql
input ImportQueueEntryInput {
  queueID: ID!
  recordingID: ID!
  fileAssignments: [FileAssignmentInput!]
}

input FileAssignmentInput {
  sourcePath: String!
  kind: String!  # "main" | "part-N" | "extra-{kind}" | "skip"
  label: String  # optional, only for extras
}
```

`fileAssignments` defaults to the classification's heuristic when
omitted (lets simple loose-file imports skip the new shape entirely).

## Ingest engine changes

`internal/ingest/ingest.go`. Current `applyPlan` builds one Plan,
moves one file, writes one version row. New flow:

1. Build a Plan per Part using the existing `{Part}` token.
2. Apply each part's Plan; write a `recording_versions` row per part
   with `part_index` set.
3. For each extra:
   - Compute the destination folder under the canonical recording
     folder per kind:
     - `featurette` → `featurettes/`
     - `scene` → `scenes/`
     - `behindthescenes` → `behindthescenes/`
     - `interview` → `interviews/`
     - `trailer` → `trailers/`
     - `deletedscenes` → `deletedscenes/`
     - `other` → `other/`
     - `audio` → `audio/`
     - `photo` → `photos/`
   - Move the file there. For directories (the Halcyon Crossing `audio/`
     subfolder case), move the whole directory.
   - Write a `recording_extras` row.
4. Empty-source-folder cleanup runs as today (commit `05b0c02`).

Per-extra failures don't abort the batch — log + continue, leave the
unhandled file in the source folder for the user to clean up
manually. Same partial-success policy as the rename apply mutation.

## NFO writer changes

For multipart: no NFO change needed. Jellyfin's split-video detection
runs on filename pattern (`... - part1.mkv`, `... - part2.mkv` in the
same folder), which our rename template already produces.

For extras: Jellyfin auto-discovers files in the conventional
subfolders (`featurettes/`, `scenes/`, etc.) without explicit NFO
hints. No movie.nfo changes required for them to appear.

Optional later: emit per-extra `<extrafanart>` or `<extrathumb>`
elements when we have art for an extra. Not in this feature's scope.

## Recording detail page

`internal/web/static/app/components/Recording.js`:

### Versions section

Today: one row per `recording_versions`. New: group rows that share
`recording_id` AND have `part_index >= 1` under a single visual row
that lists "Parts: 1, 2" + a sub-row per part. The shared metadata
(format / codec / quality) reads from part 1; the size column shows
the sum of all parts.

### Extras section

Today's `Recording.extras` GraphQL field walks `source_folder`. Replace
with a resolver that reads `recording_extras` rows. The renderer keeps
the same shape (folders collapsible, files render with size + kind
badge).

Each extra row gains a small kind badge ("Featurette", "Behind the
scenes", etc.) so the user can see at a glance what each file is
without inferring from the filename.

## CLI

`promptbook library import` (or whatever the cmd name is) extends to
accept per-file kind flags, but the SPA modal is the primary surface.
Skip CLI flag changes for v1 — the cmd path can keep using the
heuristic-suggested kinds without overrides.

## Test plan

- `internal/scanner/scanner_test.go`:
  - `TestClassifyMultipartFolder` — folder with act-1 + act-2 files
    produces `Parts: [act-1, act-2]`, no Extras, `Ambiguous=false`.
  - `TestClassifyMainPlusFeaturette` — Greenwich Beacon-shaped folder with main
    + bows.mp4 → Parts: [main], Extras: [{bows, featurette}].
  - `TestClassifyMainPlusAudioSubfolder` — Halcyon Crossing-shaped folder
    → Parts: [main], Extras with `kind=audio` for each track.
  - `TestClassifyAmbiguousVideos` — two similar-size videos at root,
    no part markers → `Ambiguous=true`.
- `internal/ingest/`:
  - `TestApplyPlan_Multipart` — two-part input → two version rows
    with correct `part_index`, both files at canonical paths.
  - `TestApplyPlan_ExtrasIntoSubfolders` — extras land in
    `featurettes/`, `audio/`, etc. under the canonical folder; matching
    `recording_extras` rows written.
  - `TestApplyPlan_PartialExtraFailure` — one extra fails to move;
    main + other extras still complete.
- `internal/server/graphql_test.go`:
  - `TestImportQueueEntryWithFileAssignments` — explicit assignments
    override the heuristic.
  - `TestRecordingExtrasFromTable` — resolver reads from
    `recording_extras` rather than walking `source_folder`.

## Suggested execution phasing

| Phase | Scope | Approx commit size |
|---|---|---|
| 1 | Schema + ingest changes (multipart parts, extras table, kind→subfolder mover, `recording_extras` resolver swap) — backend only, no UI | medium |
| 2 | Scanner classification + `manual_import_queue.classification_json` column | small-medium |
| 3 | Modal multi-file picker + `fileAssignments` mutation arg | medium |
| 4 | Recording detail polish — multipart row grouping + extras kind badges | small |

Phase 1 is the foundation; it can land without the UI changes and the
old single-file flow keeps working (it just won't take advantage of
the new fields). Phase 2 makes the queue smarter without changing the
modal. Phase 3 exposes the picker. Phase 4 is polish.

## Open questions

1. **Should the extras kind be inferable from a sidecar file** (e.g.
   `bows!.kind` saying `featurette`) so user-curated drops carry their
   own metadata? Probably not for v1 — manual UI assignment is fine
   given the low-volume usage pattern.
2. **What about "scenes" that the user doesn't want imported as extras
   — just stored as files?** The Skip kind handles that. They stay in
   the source folder and the `source_folder` field still tracks them
   for read-time enumeration if we ever want to surface "you skipped
   these on import."
3. **Multipart NFO** — Jellyfin's split-video detection should work
   off filename. Worth verifying with one real-world test before
   shipping.

## Effort estimate

Each phase is one agent task with `isolation: "worktree"`, ~30 min
per. Total: ~2 hours of agent time across four phases.
