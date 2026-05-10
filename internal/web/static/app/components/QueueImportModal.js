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
//   4. Filename preview: code block with destAbsolute. Loading
//      spinner while in flight; "Could not compute preview" message
//      on error (engine may still know more than the planner).
//   5. Footer: Cancel (ghost) + Import (primary). Import disabled
//      until a match is selected. Import errors render inline at the
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
import { humanSize, smartDate, errorMessage } from '../utils/format.js';

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

// runPreview fires the previewQueueImport query for the supplied
// queue + recording pair. Updates the local state with the result
// (or an error message). Pulled out so the in-flight handling stays
// close to the state mutations.
function runPreview(local, queueID, recordingID) {
  local.preview.loading = true;
  local.preview.error = null;
  local.preview.dest = '';
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
  const date = smartDate(rec.date_full, rec.date_month_known, rec.date_day_known);
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
  return m('pre', {
    class: 'text-xs bg-base-200 rounded p-3 overflow-x-auto',
  }, m('code', p.dest));
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

  const variables = {
    input: {
      queueID:     'queue-' + queueID,
      recordingID: 'recording-' + local.match.id,
    },
  };
  graphql.query(IMPORT_MUTATION, variables)
    .then((data) => {
      const resp = (data && data.importQueueEntry) || {};
      local.importing = false;
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
        m('div', { class: 'modal-box max-w-2xl' }, ' '));
    }

    const canImport = !!(local.match && local.match.id) && !local.importing;
    const filePath  = item.file_path || '';
    const size      = humanSize(item.file_size_bytes);

    return m('dialog', { class: 'modal' }, [
      m('div', { class: 'modal-box max-w-2xl' }, [
        // Close button at the corner.
        m('form', { method: 'dialog' },
          m('button', {
            type: 'submit',
            class: 'btn btn-sm btn-circle btn-ghost absolute right-2 top-2',
            'aria-label': 'Close',
          }, '✕')),
        m('h3', { class: 'font-bold text-lg mb-2 pr-8' }, 'Import file'),

        // File path + size header.
        m('div', { class: 'space-y-1 mb-4' }, [
          m('div', {
            class: 'font-mono text-sm truncate',
            title: filePath,
          }, filePath),
          m('div', { class: 'text-xs opacity-60 font-mono' }, size),
        ]),

        // Match section.
        m('div', { class: 'space-y-2 mb-4' }, [
          m('h4', { class: 'text-xs uppercase opacity-60 tracking-wide' }, 'Match'),
          MatchSection(local, item.id),
        ]),

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
      loading: false,
      dest:    '',
      error:   null,
    },
  };
  return local;
}

export default QueueImportModal;
