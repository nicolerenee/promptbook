// QueueImportModal.js — DaisyUI <dialog>-based modal that drives the
// manual-import flow for one queue row.
//
// Layout:
//   1. Title row + close button.
//   2. File path + size header.
//   3. Match section. Two modes:
//        - Pre-filled: rich Show / Date / Master display + "Change"
//          link that drops back into typeahead mode.
//        - Typeahead: search input (debounced 300ms) + dropdown of
//          searchRecordings results. Picking one swaps to pre-filled.
//   4. Files-in-this-folder section. Renders the queue row's
//      classification (parts + extras) with per-row Kind dropdowns —
//      Main / Part 1..5 / Featurette / Scene / Behind the scenes /
//      Interview / Trailer / Deleted scenes / Other / Photo / Audio /
//      Skip. Opens expanded when the classifier flagged the folder
//      ambiguous or surfaced multiple parts; otherwise collapses
//      behind a <details> disclosure so simple single-file folders
//      don't gain UX friction. Hidden entirely when there are no
//      extras to assign (legacy / single-loose-file imports).
//   5. Filename preview: code block with destAbsolute. Loading
//      spinner while in flight; "Could not compute preview" message
//      on error (engine may still know more than the planner).
//      Re-fires when the user picks a new Main row (or shifts the
//      part ordering) so the preview reflects the planner's input.
//   6. Footer: Cancel (ghost) + Import (primary). Import disabled
//      until a match is selected and the local validator reports
//      the file picker is in a valid shape (exactly one Main XOR
//      contiguous Part-N rows). Import errors render inline at the
//      bottom of the body so the modal stays open and the user can
//      retry without losing context.
//
// State lives on `state.queue.importingItem` (the row currently being
// imported, or null) + the per-modal local state on `attrs.local`
// the parent owns so a redraw doesn't reset typed text. The parent
// (Queue.js) clears importingItem on Cancel / Import success; the
// modal itself never mutates that field.
//
// Wire format: searchRecordings + previewQueueImport both flow
// through GraphQL. The id strings are prefixed (recording-N,
// queue-N); we strip on display only — the wire stays prefixed.

import m from 'https://esm.sh/mithril@2.2.2';
import graphql from '../graphql.js';
import { humanSize, smartDateWithVariant, errorMessage } from '../utils/format.js';

// SEARCH_QUERY drives the typeahead. Empty / short queries return
// [] (server-side guard); we still debounce so the user can type
// "Halcyon Crossing" without firing 12 round-trips.
const SEARCH_QUERY = `
  query SearchRecordings($query: String!, $limit: Int) {
    searchRecordings(query: $query, limit: $limit) {
      id
      showID
      show
      tour
      dateFull
      dateMonthKnown
      dateDayKnown
      dateVariant
      master
    }
  }
`;

// PREVIEW_QUERY runs the rename Plan side and returns the planned
// destination. Does NOT move the file; the SPA renders destAbsolute
// as a code block so the user can confirm before hitting Import.
const PREVIEW_QUERY = `
  query PreviewQueueImport($input: PreviewQueueImportInput!) {
    previewQueueImport(input: $input) {
      destFolder
      destFile
      destAbsolute
      destExists
      isDuplicate
    }
  }
`;

// IMPORT_MUTATION is the same mutation Queue.js's old Match button
// fired — moves the file, removes the row, writes a manual_import
// history event. We thread the user's chosen recording id through as
// an explicit override so the resolver doesn't fall back to the
// queue's suggestion (which may not be what the user picked).
const IMPORT_MUTATION = `
  mutation ImportQueueEntry($input: ImportQueueEntryInput!) {
    importQueueEntry(input: $input) {
      ok
      action
      dest
      error
    }
  }
`;

// stripIDPrefix turns "recording-1234" into "1234". Pulled out as a
// tiny helper so the modal stays self-contained — Queue.js has its
// own copy.
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// mapRecordingItem rewrites a GraphQL RecordingsListItem into the
// shape the modal renders against. Mirrors the projection in
// Queue.js for the suggestedRecording field so both surfaces agree
// on key names.
function mapRecordingItem(node) {
  if (!node) return null;
  return {
    id:               Number(stripIDPrefix(node.id)),
    show:             node.show || '',
    tour:             node.tour || '',
    date_full:        node.dateFull || '',
    date_month_known: !!node.dateMonthKnown,
    date_day_known:   !!node.dateDayKnown,
    date_variant:     node.dateVariant || '',
    master:           node.master || '',
  };
}

// SEARCH_DEBOUNCE_MS is the delay before a typeahead fetch fires.
// 300ms is the muscle-memory web idiom — long enough that the user
// can type a full word, short enough that the dropdown doesn't feel
// laggy.
const SEARCH_DEBOUNCE_MS = 300;

// SEARCH_LIMIT caps the dropdown size. The server caps at 25 anyway;
// pinning the client at 25 keeps the surface explicit.
const SEARCH_LIMIT = 25;

// PART_KIND_LIMIT caps how many Part-N options the kind dropdown
// surfaces. Real-world multipart drops are 2 acts (musical theatre),
// occasionally 3 for 3-act plays; offering 5 leaves headroom without
// turning the dropdown into a phonebook.
const PART_KIND_LIMIT = 5;

// FILE_KIND_OPTIONS is the canonical list of dropdown values shown in
// the modal's "Files in this folder" picker. Each entry pairs a stable
// `value` string (which doubles as the Mithril option key + a kind
// token the local validator inspects) with a user-facing label. The
// values are not the wire-format ingest.AssignmentKind* tokens — those
// get derived at submit time so the dropdown stays human-shaped (e.g.
// "main" / "part1" / "featurette") while the wire stays canonical
// (`main` / `part-1` / `extra-featurette`).
//
//nolint:gochecknoglobals — JS module-level const.
const FILE_KIND_OPTIONS = [
  { value: 'main',            label: 'Main' },
  // Part-N options are appended below so the cap stays in one place.
  { value: 'extra-featurette',     label: 'Featurette' },
  { value: 'extra-scene',          label: 'Scene' },
  { value: 'extra-behindthescenes', label: 'Behind the scenes' },
  { value: 'extra-interview',      label: 'Interview' },
  { value: 'extra-trailer',        label: 'Trailer' },
  { value: 'extra-deletedscenes',  label: 'Deleted scenes' },
  { value: 'extra-other',          label: 'Other' },
  { value: 'extra-photo',          label: 'Photo' },
  { value: 'extra-audio',          label: 'Audio' },
  { value: 'skip',                 label: 'Skip — leave in source' },
];

// Insert Part 1..N options right after Main. Done at module load
// rather than inline so the array order is unambiguous in the source.
for (let i = PART_KIND_LIMIT; i >= 1; i--) {
  FILE_KIND_OPTIONS.splice(1, 0, {
    value: 'part-' + i,
    label: 'Part ' + i,
  });
}

// shortPath collapses an absolute path to its last two segments
// (e.g. "audio/01 - Wedding Song.mp3"). The full path is kept in the
// row's title attr so hovering reveals it; the table itself stays
// readable when audio/ holds a dozen tracks.
function shortPath(path, sourceFolder) {
  if (!path) return '';
  if (sourceFolder && path.indexOf(sourceFolder) === 0) {
    let rel = path.substring(sourceFolder.length);
    if (rel.startsWith('/')) rel = rel.substring(1);
    if (rel.length > 0) return rel;
  }
  // Fall back to last two segments when no sourceFolder is supplied.
  const parts = path.split('/').filter(Boolean);
  if (parts.length <= 2) return parts.join('/');
  return parts.slice(-2).join('/');
}

// validateAssignments runs the constraint check the modal surfaces
// inline whenever a kind dropdown changes. Returns a non-empty error
// string when the picker is invalid (no main + no parts, multiple
// mains, non-contiguous parts, etc.); '' when the shape is acceptable.
// Mirrors the server-side validateAssignments rules in
// internal/ingest/assignments.go so the UI gives immediate feedback
// without round-tripping the mutation. Empty rows[] (single loose
// file, no extras) skips validation entirely — those imports flow
// through the legacy single-file path.
function validateAssignments(rows) {
  if (!rows || rows.length === 0) return '';
  let mains = 0;
  const partIndices = [];
  for (const r of rows) {
    if (r.kind === 'main') {
      mains++;
    } else if (r.kind && r.kind.indexOf('part-') === 0) {
      const idx = parseInt(r.kind.substring(5), 10);
      if (Number.isFinite(idx) && idx >= 1) partIndices.push(idx);
    }
  }
  if (mains > 0 && partIndices.length > 0) {
    return 'Pick exactly one Main, or use Part 1 / Part 2 for multipart — not both.';
  }
  if (mains === 0 && partIndices.length === 0) {
    return 'Pick exactly one Main, or use Part 1 / Part 2 for multipart.';
  }
  if (mains > 1) {
    return 'Only one row can be Main; use Part 1 / Part 2 for multipart instead.';
  }
  if (partIndices.length === 1) {
    return 'A single Part-N row is not multipart; pick Main instead.';
  }
  if (partIndices.length >= 2) {
    partIndices.sort((a, b) => a - b);
    for (let i = 0; i < partIndices.length; i++) {
      if (partIndices[i] !== i + 1) {
        return 'Part numbers must be consecutive starting at 1 (no gaps, no duplicates).';
      }
    }
  }
  return '';
}

// buildFileRows projects the queue row's classification (parts +
// extras) into a flat list of rows the modal renders. Each row
// captures `path`, `size_bytes`, the heuristic-suggested kind value
// (mapped to a FILE_KIND_OPTIONS value), and an optional user-supplied
// label. The current Main row gets recorded so the destination preview
// can re-fire whenever it shifts. Returns [] for queue rows without a
// classification (legacy or single-loose-file imports).
function buildFileRows(item) {
  const cls = item && item.classification;
  if (!cls) return [];
  const rows = [];
  for (const p of cls.parts || []) {
    rows.push({
      path:       p.path,
      size_bytes: p.size_bytes || 0,
      kind:       normalizeKindValue(p.suggested_kind),
      label:      '',
    });
  }
  for (const x of cls.extras || []) {
    rows.push({
      path:       x.path,
      size_bytes: x.size_bytes || 0,
      kind:       normalizeKindValue(x.suggested_kind),
      label:      '',
    });
  }
  return rows;
}

// normalizeKindValue maps a server-supplied suggested kind token to a
// FILE_KIND_OPTIONS value. The scanner emits canonical wire-format
// strings (`main` / `part-1` / `extra-featurette` / etc.); the modal
// accepts those as-is when they match an option, otherwise it falls
// through to `extra-other` so the row is at least selectable.
function normalizeKindValue(kind) {
  const value = kind || '';
  if (FILE_KIND_OPTIONS.some((o) => o.value === value)) return value;
  return 'extra-other';
}

// shouldExpandFiles decides whether the "Files in this folder" section
// opens expanded (vs hiding behind a <details> disclosure). True when:
//   - the classifier flagged the folder ambiguous (the heuristic
//     couldn't pick a confident main file), OR
//   - the rows include multiple Parts (multipart imports — the user
//     should confirm the order before commit).
// Otherwise the section stays collapsed; simple single-file folders
// don't gain UX friction.
function shouldExpandFiles(item, rows) {
  if (item && item.classification && item.classification.ambiguous) return true;
  let parts = 0;
  for (const r of rows || []) {
    if (r.kind && r.kind.indexOf('part-') === 0) parts++;
  }
  return parts >= 2;
}

// findMainPath returns the absolute path of the row currently chosen
// as Main, or part-1 when the picker is in multipart mode. Used by
// runPreview's re-fire decision logic — when the Main path changes
// (or the part-1 ordering shifts), the destination preview needs to
// re-run because the planner's input differs.
function findMainPath(rows) {
  if (!rows) return '';
  for (const r of rows) {
    if (r.kind === 'main') return r.path;
  }
  for (const r of rows) {
    if (r.kind === 'part-1') return r.path;
  }
  return '';
}

// fileAssignmentsForMutation builds the wire-format assignments array
// the importQueueEntry mutation accepts. Returns null for empty / no-
// classification cases so the resolver runs the legacy single-file
// flow exactly. `skip` rows are passed through to the engine so it
// knows to leave those files alone.
function fileAssignmentsForMutation(rows) {
  if (!rows || rows.length === 0) return null;
  return rows.map((r) => {
    const out = { sourcePath: r.path, kind: r.kind };
    if (r.label) out.label = r.label;
    return out;
  });
}

// runPreview fires the previewQueueImport query for the supplied
// queue + recording pair. Updates the local state with the result
// (or an error message). Pulled out so the in-flight handling stays
// close to the state mutations.
function runPreview(local, queueID, recordingID) {
  local.preview.loading = true;
  local.preview.error = null;
  local.preview.dest = '';
  local.preview.destExists = false;
  local.preview.isDuplicate = false;
  // Reset the user's overwrite confirmation whenever a fresh preview
  // fires — picking a different recording shouldn't carry forward a
  // stale "yes, overwrite" answer.
  local.overwrite = false;
  m.redraw();

  const variables = {
    input: {
      queueID:     'queue-' + queueID,
      recordingID: 'recording-' + recordingID,
    },
  };
  graphql.query(PREVIEW_QUERY, variables)
    .then((data) => {
      const payload = (data && data.previewQueueImport) || {};
      local.preview.loading = false;
      local.preview.dest = payload.destAbsolute || '';
      local.preview.destExists = !!payload.destExists;
      local.preview.isDuplicate = !!payload.isDuplicate;
      m.redraw();
    })
    .catch((err) => {
      local.preview.loading = false;
      local.preview.error = errorMessage(err);
      m.redraw();
    });
}

// runSearch fires searchRecordings with the user's current query.
// Stale completions (the user typed more by the time the request
// returns) are dropped via the lastFiredAt token so the dropdown
// always reflects the latest keystroke.
function runSearch(local, query) {
  const trimmed = (query || '').trim();
  if (trimmed.length < 2) {
    local.search.loading = false;
    local.search.results = [];
    local.search.error = null;
    m.redraw();
    return;
  }
  const fired = Date.now();
  local.search.lastFiredAt = fired;
  local.search.loading = true;
  local.search.error = null;
  m.redraw();

  graphql.query(SEARCH_QUERY, { query: trimmed, limit: SEARCH_LIMIT })
    .then((data) => {
      // Drop the response if a newer query fired in the meantime.
      if (local.search.lastFiredAt !== fired) return;
      const items = ((data && data.searchRecordings) || [])
        .map(mapRecordingItem)
        .filter(Boolean);
      local.search.loading = false;
      local.search.results = items;
      m.redraw();
    })
    .catch((err) => {
      if (local.search.lastFiredAt !== fired) return;
      local.search.loading = false;
      local.search.error = errorMessage(err);
      m.redraw();
    });
}

// scheduleSearch debounces runSearch so a fresh keystroke restarts
// the timer. The closure-captured timer handle lives on `local` so
// it survives Mithril redraws.
function scheduleSearch(local, query) {
  if (local.search.timer) {
    window.clearTimeout(local.search.timer);
    local.search.timer = null;
  }
  local.search.query = query;
  local.search.timer = window.setTimeout(() => {
    local.search.timer = null;
    runSearch(local, query);
  }, SEARCH_DEBOUNCE_MS);
}

// pickRecording sets the selected match + kicks off the preview
// fetch. Drops back out of typeahead mode so the user sees the rich
// summary without juggling the search input.
function pickRecording(local, queueID, rec) {
  local.match = rec;
  local.mode = 'prefilled';
  local.search.results = [];
  local.search.query = '';
  local.search.error = null;
  if (rec && rec.id) {
    runPreview(local, queueID, rec.id);
  } else {
    local.preview.dest = '';
    local.preview.error = null;
    local.preview.loading = false;
  }
  m.redraw();
}

// recordingSummary renders the rich Show / Date · Master line used
// in both the pre-filled match section and the typeahead dropdown.
// Aspect mirrors Recordings.js's row pattern: show name in
// font-medium, the rest in a smaller muted line.
function recordingSummary(rec) {
  if (!rec) return null;
  const subtitleParts = [];
  const date = smartDateWithVariant(
    rec.date_full, rec.date_month_known, rec.date_day_known, rec.date_variant);
  if (date && date !== '—') subtitleParts.push(date);
  if (rec.tour)   subtitleParts.push(rec.tour);
  if (rec.master) subtitleParts.push(rec.master);
  return m('div', { class: 'min-w-0' }, [
    m('div', { class: 'font-semibold truncate', title: rec.show || '' },
      rec.show || '— Unknown show —'),
    m('div', { class: 'text-sm opacity-70 truncate' },
      subtitleParts.join(' · ') || ('enc-' + rec.id)),
  ]);
}

// MatchSection renders either the pre-filled summary (with a
// "Change" affordance) or the typeahead input + dropdown. The user
// picks once; from then on the section stays pre-filled until they
// hit "Change".
function MatchSection(local, queueID) {
  if (local.mode === 'prefilled' && local.match) {
    return m('div', { class: 'space-y-2' }, [
      m('div', { class: 'flex items-start justify-between gap-3 ' +
                        'p-3 rounded-box bg-base-200' }, [
        recordingSummary(local.match),
        m('button', {
          type: 'button',
          class: 'btn btn-ghost btn-xs',
          onclick: () => {
            local.mode = 'search';
            local.match = null;
            local.preview.dest = '';
            local.preview.error = null;
            local.preview.loading = false;
            // Re-fire the last query so the dropdown isn't blank
            // on re-entry; if there was no query, leave it empty.
            if (local.search.query) runSearch(local, local.search.query);
          },
        }, 'Change'),
      ]),
    ]);
  }

  const results  = local.search.results || [];
  const loading  = !!local.search.loading;
  const errorMsg = local.search.error;

  return m('div', { class: 'space-y-2' }, [
    m('input', {
      type: 'search',
      class: 'input input-bordered w-full',
      placeholder: 'Search by show or tour…',
      value: local.search.query || '',
      autofocus: true,
      oninput: (ev) => scheduleSearch(local, ev.target.value),
    }),
    errorMsg
      ? m('div', { role: 'alert', class: 'alert alert-error text-sm' },
          m('span', 'Search failed: ' + errorMsg))
      : null,
    loading
      ? m('div', { class: 'opacity-60 text-sm py-2' },
          m('span', { class: 'loading loading-spinner loading-xs mr-2' }), 'Searching…')
      : null,
    !loading && !errorMsg && results.length === 0 && local.search.query
      ? m('div', { class: 'opacity-60 text-sm py-2' },
          'No recordings match "' + local.search.query + '".')
      : null,
    results.length > 0
      ? m('ul', { class: 'menu bg-base-200 rounded-box max-h-72 ' +
                          'overflow-y-auto p-1 gap-0' },
          results.map((rec) => m('li', { key: rec.id }, m('button', {
            type: 'button',
            class: 'flex items-start text-left w-full gap-2 py-2',
            onclick: () => pickRecording(local, queueID, rec),
          }, recordingSummary(rec)))))
      : null,
  ]);
}

// PreviewSection renders the planned destination path. While the
// preview is in flight we show a skeleton-style spinner; on error we
// show a muted "Could not compute preview" line so the user knows
// the field is intentional, not stuck. The Import button is NOT
// blocked by a preview error — the engine may know more than the
// planner about the resolved id, e.g. it'll auto-fetch a missing
// recording.
function PreviewSection(local) {
  const p = local.preview;
  if (!local.match) {
    return m('div', { class: 'opacity-60 text-sm italic' },
      'Pick a recording above to see the planned destination.');
  }
  if (p.loading) {
    return m('div', { class: 'flex items-center gap-2 text-sm opacity-70' }, [
      m('span', { class: 'loading loading-spinner loading-xs' }),
      'Computing preview…',
    ]);
  }
  if (p.error) {
    return m('div', { class: 'text-sm opacity-70' }, [
      'Could not compute preview: ',
      m('span', { class: 'opacity-60' }, p.error),
    ]);
  }
  if (!p.dest) {
    return m('div', { class: 'opacity-60 text-sm italic' },
      'No preview available.');
  }
  return m('div', { class: 'space-y-2' }, [
    m('pre', {
      class: 'text-xs bg-base-200 rounded p-3 overflow-x-auto',
    }, m('code', p.dest)),
    ConflictBanner(local),
  ]);
}

// ConflictBanner renders the destination-conflict warning when the
// preview reported destExists. Three states:
//
//   - destExists + isDuplicate → info banner: "already imported, this
//     is a duplicate" with a soft "Importing will close this modal
//     and remove the queue row" hint. The Import button still works
//     (the mutation handles the no-op).
//   - destExists + !isDuplicate + !overwrite → warning banner with an
//     overwrite checkbox. Import button is disabled until the user
//     ticks overwrite.
//   - destExists + !isDuplicate + overwrite → warning banner reads
//     "you've confirmed overwrite, will replace existing file."
//   - !destExists → no banner.
function ConflictBanner(local) {
  const p = local.preview;
  if (!p || !p.destExists) return null;
  if (p.isDuplicate) {
    return m('div', {
      role: 'alert',
      class: 'alert alert-info text-sm',
    }, m('div', [
      m('span', { class: 'font-semibold mr-1' }, 'Duplicate detected.'),
      'An identical file (same size + checksum) already exists at the destination. ' +
      'Importing will mark the queue row as handled and leave both files in place.',
    ]));
  }
  return m('div', {
    role: 'alert',
    class: 'alert alert-warning text-sm flex flex-col items-start gap-2',
  }, [
    m('div', [
      m('span', { class: 'font-semibold mr-1' }, 'Destination is occupied.'),
      'A different file already exists at this path. ',
      'Importing will replace it.',
    ]),
    m('label', { class: 'flex items-center gap-2 cursor-pointer' }, [
      m('input', {
        type: 'checkbox',
        class: 'checkbox checkbox-sm',
        checked: !!local.overwrite,
        onchange: (ev) => { local.overwrite = !!ev.target.checked; },
      }),
      m('span', 'I understand — overwrite the existing file'),
    ]),
  ]);
}

// onKindChange runs whenever the user picks a new value in a Files
// table dropdown. Re-runs the local validator + decides whether to
// refire the destination preview. The preview is the most expensive
// query in the modal; we only re-fire it when the change shifts the
// "main path" the planner consumes (a row becoming Main, leaving
// Main, swapping which row is part-1 — anything that affects the
// canonical destination). Pure extra-kind toggles (Featurette ↔
// Photo) never shift the Main path so they skip the round-trip.
function onKindChange(local, queueID, rowIndex, newValue) {
  if (!local.files || !local.files.rows[rowIndex]) return;
  const beforeMain = findMainPath(local.files.rows);
  local.files.rows[rowIndex].kind = newValue;
  const afterMain = findMainPath(local.files.rows);
  local.files.error = validateAssignments(local.files.rows);
  if (
    beforeMain !== afterMain &&
    local.match && local.match.id
  ) {
    runPreview(local, queueID, local.match.id);
  }
  m.redraw();
}

// FilesSection renders the per-folder multi-file picker. Open expanded
// when the classifier flagged the folder ambiguous or surfaced
// multiple parts; otherwise collapsed behind a <details> disclosure
// (`click to override`) so single-file folders don't gain UX friction.
// The section is hidden entirely when there are no extras to assign
// (empty rows[]).
function FilesSection(local, item) {
  const rows = (local.files && local.files.rows) || [];
  if (rows.length === 0) return null;
  const expanded = local.files.expanded;
  const summary = m('summary', {
    class: 'cursor-pointer text-xs uppercase opacity-60 tracking-wide ' +
           'list-none flex items-center gap-2 select-none',
  }, [
    m('span', 'Files in this folder · ' + rows.length),
    m('span', { class: 'opacity-60 normal-case font-normal lowercase' },
      '· click to override'),
  ]);
  const table = renderFilesTable(local, item, rows);
  const errorLine = local.files.error
    ? m('div', { class: 'text-error text-sm mt-2' }, local.files.error)
    : null;
  if (expanded) {
    return m('div', { class: 'space-y-2' }, [
      m('h4', { class: 'text-xs uppercase opacity-60 tracking-wide' },
        'Files in this folder · ' + rows.length),
      table,
      errorLine,
    ]);
  }
  return m('details', { class: 'space-y-2' }, [
    summary,
    m('div', { class: 'mt-2' }, [table, errorLine]),
  ]);
}

// renderFilesTable is the actual <table> the FilesSection wraps.
// Each row carries the relative path (full path on hover), a kind
// dropdown, and the file size. Pulled out so the expanded / collapsed
// branches in FilesSection share one renderer.
function renderFilesTable(local, item, rows) {
  const sourceFolder = sourceFolderForItem(item);
  return m('table', { class: 'table table-sm w-full' }, [
    m('thead', m('tr', [
      m('th', { class: 'text-xs uppercase opacity-60' }, 'File'),
      m('th', { class: 'text-xs uppercase opacity-60 w-44' }, 'Kind'),
      m('th', { class: 'text-xs uppercase opacity-60 w-24 text-right' }, 'Size'),
    ])),
    m('tbody', rows.map((row, idx) => m('tr', { key: row.path }, [
      m('td', {
        class: 'font-mono text-xs break-all',
        title: row.path,
      }, shortPath(row.path, sourceFolder)),
      m('td',
        m('select', {
          class: 'select select-bordered select-xs w-full',
          value: row.kind,
          onchange: (ev) => onKindChange(local, item.id, idx, ev.target.value),
        }, FILE_KIND_OPTIONS.map((opt) => m('option', {
          key:   opt.value,
          value: opt.value,
        }, opt.label)))),
      m('td', { class: 'text-xs opacity-70 text-right whitespace-nowrap' },
        humanSize(row.size_bytes)),
    ]))),
  ]);
}

// sourceFolderForItem returns the folder under which the queue row's
// classification entries live. Used by shortPath to relativize each
// row's display path. Falls back to the dirname of file_path so loose
// classifications (where the row's main file shares a parent with the
// extras) still get the right common prefix.
function sourceFolderForItem(item) {
  if (!item) return '';
  const fp = item.file_path || '';
  const idx = fp.lastIndexOf('/');
  return idx >= 0 ? fp.substring(0, idx) : '';
}

// runImport fires the importQueueEntry mutation with the user's
// chosen recording. On success the parent's onSuccess removes the
// row + closes the modal; on failure we surface the error inline so
// the modal stays open for retry.
function runImport(local, queueID, onSuccess) {
  if (!local.match || !local.match.id) return;
  local.importing = true;
  local.importError = null;
  m.redraw();

  const fileRows  = (local.files && local.files.rows) || [];
  const assignments = fileAssignmentsForMutation(fileRows);
  const variables = {
    input: {
      queueID:     'queue-' + queueID,
      recordingID: 'recording-' + local.match.id,
      overwrite:   !!local.overwrite,
    },
  };
  if (assignments && assignments.length > 0) {
    variables.input.fileAssignments = assignments;
  }
  graphql.query(IMPORT_MUTATION, variables)
    .then((data) => {
      const resp = (data && data.importQueueEntry) || {};
      local.importing = false;
      // Duplicate path: server already removed the queue row + left
      // both files alone. Treat as a "soft success" — close the modal
      // and let the parent drop the row from local state via
      // onSuccess. The action string lets the caller surface a
      // distinct toast if it wants.
      if (resp.action === 'duplicate') {
        onSuccess(resp);
        return;
      }
      if (resp.ok) {
        onSuccess(resp);
        return;
      }
      local.importError = resp.error || 'Import failed.';
      m.redraw();
    })
    .catch((err) => {
      local.importing = false;
      local.importError = errorMessage(err);
      m.redraw();
    });
}

// QueueImportModal mirrors ImagePickerModal's <dialog> lifecycle —
// showModal()/close() driven off attrs.open. The native 'close' event
// (Esc, backdrop click, ✕) forwards to attrs.onClose so the parent's
// state.queue.importingItem flag flips back to null.
const QueueImportModal = {
  oncreate(vnode) {
    const dom = vnode.dom;
    dom.addEventListener('close', () => {
      if (typeof vnode.attrs.onClose === 'function') {
        vnode.attrs.onClose();
        m.redraw();
      }
    });
    if (vnode.attrs.open && !dom.open) dom.showModal();
  },

  onupdate(vnode) {
    const dom = vnode.dom;
    if (vnode.attrs.open && !dom.open) dom.showModal();
    else if (!vnode.attrs.open && dom.open) dom.close();
  },

  view(vnode) {
    const a     = vnode.attrs;
    const item  = a.item;
    const local = a.local;

    // Render an empty <dialog> when no item is selected so the
    // browser-managed open state doesn't fight a stale render.
    if (!item || !local) {
      return m('dialog', { class: 'modal' },
        m('div', { class: 'modal-box max-w-4xl' }, ' '));
    }

    // canImport gates the primary button. Overwrite-required (the
    // destination has different content) blocks until the user
    // explicitly checks the overwrite confirmation. Duplicate +
    // no-conflict states leave the button enabled — the mutation
    // handles the duplicate path itself.
    const overwriteBlocked = local.preview &&
      local.preview.destExists &&
      !local.preview.isDuplicate &&
      !local.overwrite;
    // filesInvalid is the local validator's verdict on the multi-file
    // picker. Empty (no rows / no error) leaves the button alone; a
    // non-empty error string blocks Import until the user fixes the
    // assignments. The same constraints get re-checked server-side, so
    // this is just fast feedback.
    const filesInvalid = !!(local.files && local.files.error);
    const canImport = !!(local.match && local.match.id) &&
      !local.importing && !overwriteBlocked && !filesInvalid;
    const filePath  = item.file_path || '';
    const size      = humanSize(item.file_size_bytes);
    // Split the path so the filename gets prominent rendering and the
    // directory hangs out underneath in muted text. The user looks at
    // the filename a lot when verifying a match — burying it inside a
    // truncated full path with hover-to-reveal was friction.
    const slashIdx  = filePath.lastIndexOf('/');
    const fileName  = slashIdx >= 0 ? filePath.substring(slashIdx + 1) : filePath;
    const dirPath   = slashIdx >= 0 ? filePath.substring(0, slashIdx) : '';

    return m('dialog', { class: 'modal' }, [
      m('div', { class: 'modal-box max-w-4xl' }, [
        // Close button at the corner.
        m('form', { method: 'dialog' },
          m('button', {
            type: 'submit',
            class: 'btn btn-sm btn-circle btn-ghost absolute right-2 top-2',
            'aria-label': 'Close',
          }, '✕')),
        m('h3', { class: 'font-bold text-lg mb-2 pr-8' }, 'Import file'),

        // Filename + size + directory header. The filename gets the
        // prominent line because the user reads it most when verifying
        // a match. extras_count surfaces alongside so the user is
        // reminded the folder also holds companion files — only the
        // main file moves on import.
        m('div', { class: 'space-y-1 mb-4' }, [
          m('div', { class: 'flex items-start gap-2 min-w-0' }, [
            m('div', {
              class: 'font-mono text-sm break-all flex-1',
            }, fileName),
            item.extras_count > 0
              ? m('span', {
                  class: 'badge badge-ghost badge-sm shrink-0 mt-0.5',
                  title: 'Other media files in the same folder. ' +
                         'Only the main file imports; extras stay in place.',
                }, '+' + item.extras_count + ' extra' +
                   (item.extras_count === 1 ? '' : 's'))
              : null,
          ]),
          m('div', { class: 'text-xs opacity-60 font-mono' }, size),
          dirPath
            ? m('div', {
                class: 'text-xs opacity-60 font-mono break-all',
              }, dirPath)
            : null,
        ]),

        // Match section.
        m('div', { class: 'space-y-2 mb-4' }, [
          m('h4', { class: 'text-xs uppercase opacity-60 tracking-wide' }, 'Match'),
          MatchSection(local, item.id),
        ]),

        // Files-in-this-folder section. Renders nothing when the queue
        // row has no classification (legacy / single-loose-file imports);
        // the legacy single-file flow handles those.
        local.files && local.files.rows.length > 0
          ? m('div', { class: 'mb-4' }, FilesSection(local, item))
          : null,

        // Filename preview.
        m('div', { class: 'space-y-2 mb-4' }, [
          m('h4', { class: 'text-xs uppercase opacity-60 tracking-wide' }, 'Destination'),
          PreviewSection(local),
        ]),

        // Inline import error.
        local.importError
          ? m('div', { role: 'alert', class: 'alert alert-error text-sm mb-2' },
              m('span', 'Import failed: ' + local.importError))
          : null,

        // Footer.
        m('div', { class: 'modal-action' }, [
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: local.importing,
            onclick: () => {
              if (typeof a.onClose === 'function') a.onClose();
            },
          }, 'Cancel'),
          m('button', {
            type: 'button',
            class: 'btn btn-primary btn-sm',
            disabled: !canImport,
            onclick: () => runImport(local, item.id, a.onImported),
          }, [
            local.importing
              ? m('span', { class: 'loading loading-spinner loading-xs mr-1' })
              : null,
            local.importing ? 'Importing…' : 'Import',
          ]),
        ]),
      ]),
      // Backdrop form — clicking outside the modal-box closes the
      // dialog. The native 'close' event then fires, which oncreate
      // forwarded above.
      m('form', { method: 'dialog', class: 'modal-backdrop' },
        m('button', { type: 'submit', 'aria-label': 'Close' }, 'close')),
    ]);
  },
};

// makeLocalState builds the initial per-modal state for one queue
// row. Exported so Queue.js can stash it on state.queue alongside
// the importingItem pointer; passing the same object through to the
// modal's `local` attr keeps user-typed text alive across redraws.
//
// item.suggestedRecording (the rich shape from the queue resolver),
// when present, pre-fills the match — the user only needs to
// confirm. Otherwise the modal opens in typeahead mode with an empty
// query.
export function makeLocalState(item) {
  const hasSuggestion = !!(item && item.suggested_recording);
  const fileRows = buildFileRows(item);
  const local = {
    mode:         hasSuggestion ? 'prefilled' : 'search',
    match:        hasSuggestion ? item.suggested_recording : null,
    importing:    false,
    importError:  null,
    search:       {
      query:        '',
      results:      [],
      loading:      false,
      error:        null,
      timer:        null,
      lastFiredAt:  0,
    },
    preview: {
      loading:     false,
      dest:        '',
      error:       null,
      destExists:  false,
      isDuplicate: false,
    },
    // overwrite is the user's explicit confirmation when the preview
    // reports destExists + !isDuplicate. The Import button stays
    // disabled until they check the box in the conflict banner.
    overwrite: false,
    // files holds the multi-file picker's per-row state. rows[] is
    // empty for queue entries with no classification (legacy /
    // single-loose-file imports); the FilesSection renderer skips the
    // entire section in that case. expanded controls whether the
    // section opens visible by default (multipart / ambiguous) or
    // hides behind a <details> disclosure.
    files: {
      rows:     fileRows,
      expanded: shouldExpandFiles(item, fileRows),
      error:    validateAssignments(fileRows),
    },
  };
  // Pre-fill case: kick off the destination preview immediately so
  // the user sees the planned path on first paint instead of an empty
  // panel. pickRecording handles the same kick-off when the user
  // picks a match via the typeahead.
  if (hasSuggestion && item && item.id && local.match && local.match.id) {
    runPreview(local, item.id, local.match.id);
  }
  return local;
}

export default QueueImportModal;
