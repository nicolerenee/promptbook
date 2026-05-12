// Queue.js — Mithril port of the legacy /static/queue.js, redesigned
// in wave 2.5 to expose a richer manual-import flow.
//
// Layout:
//   - Page header (title + sub-text + cosmetic action stubs).
//   - Three-stat tile cluster (discovered / auto-resolvable / needs you).
//   - Sortable table. Each row carries a rich Suggested-match cell
//     (show name + date · master) when the scanner had a high-
//     confidence guess; otherwise a muted "pick a recording"
//     placeholder. The whole row is clickable — opens the
//     QueueImportModal for that file.
//   - Footer hint about the [encora-N] auto-import shortcuts.
//
// The Match / Resolve buttons that lived on the right of each row
// are gone — the modal owns the entire import flow now (including a
// typeahead picker for low-confidence rows + a planner-side dest
// preview before committing).
//
// Wire format: GraphQL prefixed string ids (queue-N, recording-N)
// are stripped at the boundary; the SPA holds bare ints in
// state.queue.items so the existing renderer sort + key paths stay
// untouched. The new suggestedRecording rich field is unwrapped into
// snake_case to match the rest of the row shape.
//
// Behavior parity notes:
//   - Re-scan triggers the scan-incoming scheduled job + refetches
//     the queue after a brief delay. Import-all-auto-resolved stays
//     a cosmetic stub until a future wave wires it.
//   - Items are sorted newest-first by discovered_at, matching the
//     legacy display order.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import graphql from '../graphql.js';
import state from '../state.js';
import {
  humanSize,
  smartDateWithVariant,
  relativeTime,
  errorMessage,
} from '../utils/format.js';
import QueueImportModal, { makeLocalState } from './QueueImportModal.js';

// CONF_META keys on the lowercase API tokens. high → success (auto),
// medium → warning, low → error. Maps to DaisyUI badge color classes
// per the design doc.
const CONF_META = {
  high:   { label: 'High',   badge: 'badge-success' },
  medium: { label: 'Medium', badge: 'badge-warning' },
  low:    { label: 'Low',    badge: 'badge-error' },
};

// QUEUE_QUERY pulls every column the row renderer + sort keys need.
// suggestedRecording surfaces the rich match summary the table
// renders inline (show / date / master) so the page is one round-
// trip per refresh. classification carries the scanner's per-file
// role suggestions for folder-as-unit drops so the import modal can
// render its multi-file picker without a follow-up round-trip.
const QUEUE_QUERY = `
  query Queue {
    queue {
      id
      filePath
      fileSizeBytes
      discoveredAt
      lastSeenAt
      suggestedRecordingID
      suggestedConfidence
      notes
      extrasCount
      suggestedRecording {
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
      classification {
        ambiguous
        discFormat
        parts {
          path
          sizeBytes
          suggestedKind
          partIndex
        }
        extras {
          path
          sizeBytes
          suggestedKind
          partIndex
        }
      }
    }
  }
`;

// stripIDPrefix turns "queue-1234" / "recording-N" into the bare
// numeric id the existing renderer + router consume.
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// mapSuggestedRecording rewrites the GraphQL RecordingsListItem nested
// under QueueEntry.suggestedRecording into the snake_case shape the
// modal + row renderer share. Returns null when the field is null
// (no suggestion present, or the suggestion points at a stale row).
function mapSuggestedRecording(node) {
  if (!node) return null;
  return {
    id:               Number(stripIDPrefix(node.id)),
    show_id:          Number(stripIDPrefix(node.showID)),
    show:             node.show || '',
    tour:             node.tour || '',
    date_full:        node.dateFull || '',
    date_month_known: !!node.dateMonthKnown,
    date_day_known:   !!node.dateDayKnown,
    date_variant:     node.dateVariant || '',
    master:           node.master || '',
  };
}

// mapClassifiedFile rewrites one QueueClassifiedFile into the snake_case
// shape the modal's multi-file picker consumes. Mirrors the GraphQL
// type's field set verbatim — path, size, the heuristic-suggested kind
// token, and (for parts) the 1-based ordinal.
function mapClassifiedFile(node) {
  if (!node) return null;
  return {
    path:           node.path || '',
    size_bytes:     node.sizeBytes || 0,
    suggested_kind: node.suggestedKind || '',
    part_index:     node.partIndex || 0,
  };
}

// mapClassification rewrites the QueueClassification subfield into a
// flat snake_case object the modal owns. parts/extras are always
// non-null arrays (server-side guarantee); ambiguous defaults to
// false. Returns null when the input is null so legacy rows that
// pre-date the classifier project no classification.
//
// discFormat carries the scanner's disc-shape marker — empty string
// for normal folders, "dvd" when a VIDEO_TS layout was detected.
// The modal's FilesSection reads this to render a small DVD badge
// so the user knows the importer will preserve the VIDEO_TS
// structure rather than running the file template.
function mapClassification(node) {
  if (!node) return null;
  const parts  = ((node.parts  || []).map(mapClassifiedFile).filter(Boolean));
  const extras = ((node.extras || []).map(mapClassifiedFile).filter(Boolean));
  return {
    ambiguous:   !!node.ambiguous,
    discFormat:  node.discFormat || '',
    parts,
    extras,
  };
}

// mapQueueItem rewrites a GraphQL QueueEntry into the snake_case shape
// the legacy renderer was written against. id + suggested_recording_id
// fall back to bare integers; suggested_recording carries the rich
// summary (or null) used by the table cell + the modal's pre-fill.
// extras_count is the number of OTHER media files in the row's source
// folder (folder-as-unit drops); 0 for loose-file rows.
// classification carries the scanner's per-file role suggestions for
// the modal's multi-file picker; null for legacy rows that pre-date
// phase 2's classifier.
function mapQueueItem(node) {
  if (!node) return null;
  const out = {
    id:                   Number(stripIDPrefix(node.id)),
    file_path:            node.filePath || '',
    file_size_bytes:      node.fileSizeBytes || 0,
    discovered_at:        node.discoveredAt || '',
    last_seen_at:         node.lastSeenAt || '',
    suggested_confidence: node.suggestedConfidence || '',
    notes:                node.notes || '',
    extras_count:         node.extrasCount || 0,
    suggested_recording:  mapSuggestedRecording(node.suggestedRecording),
    classification:       mapClassification(node.classification),
  };
  if (node.suggestedRecordingID) {
    out.suggested_recording_id = Number(stripIDPrefix(node.suggestedRecordingID));
  } else {
    out.suggested_recording_id = null;
  }
  return out;
}

// triggerScan POSTs the supplied scheduled-job + refetches the queue
// once the run has had a moment to land. The job is async — RunNow
// returns immediately with a run_id — so we wait briefly before
// refetching so a typical small-incoming-dir scan has time to write
// its rows. The busy / error keys are read off `state.queue` via the
// supplied (busyKey, errorKey) so a single helper drives both the
// Re-scan and Scan library buttons. The button enters a busy state
// for the duration so a rapid double-click is a no-op.
function triggerScan(jobName, busyKey, errorKey) {
  const q = state.queue;
  if (q[busyKey]) return;
  q[busyKey] = true;
  q[errorKey] = null;
  m.redraw();

  api.post('/jobs/scheduled/' + jobName + '/run', {})
    .then(() => new Promise((resolve) => setTimeout(resolve, 1500)))
    .then(() => loadQueue())
    .catch((err) => {
      // 503 (jobs not configured) and 409 (already running) get a
      // brief inline message rather than an alert; everything else
      // surfaces the underlying error text the same way.
      if (err && err.status === 409) {
        q[errorKey] = 'A scan is already running.';
      } else if (err && err.status === 503) {
        q[errorKey] = 'Scanner is not configured.';
      } else {
        q[errorKey] = errorMessage(err);
      }
    })
    .then(() => {
      q[busyKey] = false;
      m.redraw();
    });
}

// triggerRescan fires the scan-incoming job (the watched-folder
// poller) — the legacy "Re-scan" button.
function triggerRescan() {
  triggerScan('scan-incoming', 'rescanning', 'rescanError');
}

// triggerScanLibrary fires the scan-library-root job — the manual
// orphan-backfill pass over library.root. Same async lifecycle as
// triggerRescan; the two keep distinct busy / error keys so a
// concurrent click on one doesn't blank out the other's spinner.
function triggerScanLibrary() {
  triggerScan('scan-library-root', 'scanningLibrary', 'scanLibraryError');
}

// loadQueue fetches the queue list and stores it in shared state. Items
// are sorted newest-first so the most recent drops show at the top.
function loadQueue() {
  const q = state.queue;
  q.loading = true;
  q.error = null;
  return graphql.query(QUEUE_QUERY).then((data) => {
    const items = ((data && data.queue) || []).map(mapQueueItem).filter(Boolean);
    items.sort((a, b) =>
      Date.parse(b.discovered_at || 0) - Date.parse(a.discovered_at || 0));
    q.items = items;
    q.loading = false;
  }).catch((err) => {
    q.error = err;
    q.loading = false;
  });
}

// countMetrics tallies the three header tiles. Anything that isn't a
// `high` confidence match counts as "needs you".
function countMetrics(items) {
  const out = { discovered: items.length, autoResolvable: 0, needsYou: 0 };
  items.forEach((it) => {
    if (it.suggested_confidence === 'high') out.autoResolvable++;
    else out.needsYou++;
  });
  return out;
}

// removeRow drops the queue entry with the supplied id from local state
// after a successful import. Mithril's auto-redraw triggers on the
// next mutation cycle so the table re-renders without it.
function removeRow(queueID) {
  state.queue.items = state.queue.items.filter((it) =>
    String(it.id) !== String(queueID));
}

// openImportModal pins the row + a fresh local-state object onto
// state.queue so the modal renders open with the per-row context.
// Storing local state here (rather than rebuilding it on every
// redraw) keeps user-typed text alive while the modal is open.
function openImportModal(item) {
  state.queue.importingItem = item;
  state.queue.importingLocal = makeLocalState(item);
  m.redraw();
}

// closeImportModal clears the per-row context so the dialog closes.
// The local state is dropped so the next open starts from a clean
// slate — leaving stale typeahead results around would be confusing.
function closeImportModal() {
  state.queue.importingItem = null;
  state.queue.importingLocal = null;
}

// onImportSucceeded fires after the modal's mutation returns ok=true
// (or action="duplicate"). Drops the row from local state, captures
// a "view recording" toast pointing at the recording the user just
// imported, then closes the modal. The toast is the only post-import
// affordance for hopping straight to the new recording's detail
// page; the user previously had to navigate back through the
// recordings list which lost a click.
function onImportSucceeded(item, resp) {
  // Read the chosen match BEFORE closeImportModal clears
  // importingLocal. The match always carries id + show; the modal's
  // typeahead lets the user override the queue's suggested
  // recording, so we can't rely on item.suggested_recording.
  const local = state.queue.importingLocal;
  const match = (local && local.match) || item.suggested_recording || {};
  if (match.id) {
    state.queue.successToast = {
      recordingID: match.id,
      show: match.show || 'Recording',
      duplicate: !!(resp && resp.action === 'duplicate'),
    };
  }
  removeRow(item.id);
  closeImportModal();
}

// MetricTile renders one DaisyUI `stat` block. Mirrors the helper from
// the legacy queue page; kept local so each page can tune its tone
// classes without leaking visual tokens through utils/.
function MetricTile(label, num, sub, valueClass) {
  return m('div', { class: 'stat' }, [
    m('div', { class: 'stat-title' }, label),
    m('div', { class: 'stat-value text-2xl' + (valueClass ? ' ' + valueClass : '') },
      String(num)),
    m('div', { class: 'stat-desc' }, sub),
  ]);
}

// ConfidenceBadge renders the colored chip for a row's suggested
// confidence, or an em-dash placeholder when the field is empty.
function ConfidenceBadge(conf) {
  const meta = CONF_META[conf];
  if (!meta) return m('span', { class: 'opacity-60' }, '—');
  return m('span', { class: 'badge ' + meta.badge }, meta.label);
}

// SuggestedMatchCell renders the "Suggested match" column. When the
// resolver populated the rich summary we render a two-line block
// (show in font-medium, date · master in a smaller muted line) that
// mirrors the row pattern used in Recordings.js. Otherwise show a
// muted placeholder so the user knows the row needs them to pick.
function SuggestedMatchCell(item) {
  const rec = item.suggested_recording;
  if (!rec) {
    return m('span', { class: 'opacity-60' }, 'Not matched');
  }
  const subtitleParts = [];
  const date = smartDateWithVariant(
    rec.date_full, rec.date_month_known, rec.date_day_known, rec.date_variant);
  if (date && date !== '—') subtitleParts.push(date);
  if (rec.tour)   subtitleParts.push(rec.tour);
  if (rec.master) subtitleParts.push(rec.master);
  return m('div', { class: 'min-w-0' }, [
    m('div', { class: 'font-medium truncate', title: rec.show || '' },
      rec.show || '— Unknown show —'),
    m('div', { class: 'text-xs opacity-60 truncate' },
      subtitleParts.join(' · ') || ('enc-' + rec.id)),
  ]);
}

// Row renders one queue entry. The whole row is clickable — clicks
// route to openImportModal — so users can open the modal without
// hunting for a button. The disabled selection checkbox stays
// because a future bulk-action wave will use it; clicking the cell
// stops propagation so toggling the (eventual) checkbox doesn't
// accidentally open the modal.
function Row(item) {
  return m('tr', {
    key: item.id,
    class: 'cursor-pointer hover:bg-base-300',
    onclick: () => openImportModal(item),
  }, [
    m('td', {
      onclick: (ev) => ev.stopPropagation(),
    }, m('input', {
      type: 'checkbox', class: 'checkbox checkbox-sm',
      disabled: true, title: '(coming soon)',
    })),
    m('td', { class: 'font-mono text-sm whitespace-nowrap' },
      relativeTime(item.discovered_at)),
    m('td', ConfidenceBadge(item.suggested_confidence)),
    m('td', SuggestedMatchCell(item)),
    m('td',
      m('div', { class: 'flex items-center gap-2 min-w-0' }, [
        m('span', {
          class: 'font-mono text-sm truncate max-w-[480px]',
          title: item.file_path || '',
        }, item.file_path || ''),
        item.extras_count > 0
          ? m('span', {
              class: 'badge badge-ghost badge-sm shrink-0',
              title: 'Other media files in the same folder. ' +
                     'Only the main file imports; extras stay in place.',
            }, '+' + item.extras_count + ' extra' +
               (item.extras_count === 1 ? '' : 's'))
          : null,
      ])),
    m('td', { class: 'font-mono text-sm whitespace-nowrap' },
      humanSize(item.file_size_bytes)),
  ]);
}

const Queue = {
  oninit() { loadQueue(); },

  view() {
    const q = state.queue;
    const items = q.items;

    if (q.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading queue…');
    }
    if (q.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load queue: ' + errorMessage(q.error)),
      ]);
    }

    const metrics = countMetrics(items);
    const hasAuto = metrics.autoResolvable > 0;
    const subText =
      'Watching incoming · polled every minute · ' +
      items.length + ' entr' + (items.length === 1 ? 'y' : 'ies');

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Queue'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        // Action buttons. Re-scan fires the scan-incoming job (the
        // watched-folder poller); Scan library fires the manual
        // scan-library-root pass that backfills orphan recordings
        // already living under library.root into the queue.
        // Import-all-auto-resolved is still a stub.
        m('div', { class: 'flex items-center gap-2' }, [
          state.queue.rescanError
            ? m('span', { class: 'text-xs text-error mr-2' },
                state.queue.rescanError)
            : null,
          state.queue.scanLibraryError
            ? m('span', { class: 'text-xs text-error mr-2' },
                state.queue.scanLibraryError)
            : null,
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: !!state.queue.rescanning,
            onclick: triggerRescan,
          }, state.queue.rescanning
            ? [m('span', { class: 'loading loading-spinner loading-xs' }),
               'Re-scanning…']
            : 'Re-scan'),
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: !!state.queue.scanningLibrary,
            onclick: triggerScanLibrary,
            title: 'Scan library.root for orphan recordings not yet ' +
                   'tracked in promptbook',
          }, state.queue.scanningLibrary
            ? [m('span', { class: 'loading loading-spinner loading-xs' }),
               'Scanning library…']
            : 'Scan library'),
          m('button', {
            type: 'button',
            class: 'btn btn-primary btn-sm',
            disabled: !hasAuto,
            title: '(coming soon)',
          }, 'Import all auto-resolved'),
        ]),
      ]),

      // Metric tiles. text-success / text-warning tint the auto vs.
      // needs-you tiles to match the legacy pages' status colors.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Discovered',      metrics.discovered,      'in queue'),
        MetricTile('Auto-resolvable', metrics.autoResolvable,  'high-confidence match', 'text-success'),
        MetricTile('Needs you',       metrics.needsYou,        'awaiting review',       'text-warning'),
      ]),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table' }, [
          m('thead', m('tr', [
            m('th', { style: 'width:36px' }),
            m('th', 'Discovered'),
            m('th', 'Confidence'),
            m('th', 'Suggested match'),
            m('th', 'File'),
            m('th', 'Size'),
          ])),
          m('tbody', items.length === 0
            ? m('tr', m('td', {
                colspan: 6, class: 'text-center opacity-60 py-8',
              }, [
                'Queue is empty. Drop video files into your ',
                m('code', 'library.incomingDirs'),
                ' to see them here.',
              ]))
            : items.map(Row)),
        ])),

      // Footer hint about auto-import shortcuts.
      m('p', { class: 'text-xs opacity-60 italic' }, [
        'Files matching ', m('code', '[encora-NNNNN]'),
        ' in their name auto-import on the next scan. Drop a ',
        m('code', '.encora-id'),
        ' sidecar next to a video to add the ID without renaming.',
      ]),

      // Per-row import modal. Renders an empty placeholder dialog
      // when no row is open; the modal component handles the dialog
      // lifecycle (showModal / close) off the `open` flag.
      m(QueueImportModal, {
        open:       !!q.importingItem,
        item:       q.importingItem,
        local:      q.importingLocal,
        onClose:    closeImportModal,
        onImported: (resp) => onImportSucceeded(q.importingItem, resp),
      }),

      // Post-import success toast. Pinned bottom-right with a
      // "View recording" link so the user can hop straight to the
      // newly-imported recording's detail page without bouncing
      // through the recordings list.
      renderImportSuccessToast(q.successToast),
    ]);
  },
};

// renderImportSuccessToast surfaces state.queue.successToast as a
// DaisyUI alert-success toast with a dismiss button + a "View
// recording" link. Clicking the link routes via Mithril's router so
// the SPA stays a single load. Dismiss clears the state field; the
// toast also auto-dismisses after 8 seconds via a setTimeout the
// view fires once on first render of a given toast.
function renderImportSuccessToast(toast) {
  if (!toast) return null;
  // Schedule a one-shot auto-dismiss the first time we render this
  // particular toast (different recordingID → different toast).
  // Mithril fires view() on every redraw so we gate on a sentinel
  // attached to the toast object itself.
  if (!toast.timerScheduled) {
    toast.timerScheduled = true;
    setTimeout(() => {
      if (state.queue.successToast === toast) {
        state.queue.successToast = null;
        m.redraw();
      }
    }, 8000);
  }
  const dismiss = () => { state.queue.successToast = null; };
  const goToRecording = (ev) => {
    ev.preventDefault();
    state.queue.successToast = null;
    m.route.set('/recordings/' + toast.recordingID);
  };
  const headline = toast.duplicate
    ? 'Already imported'
    : 'Imported · ' + toast.show;
  return m('div', { class: 'toast toast-end z-50' }, [
    m('div', { role: 'status', class: 'alert alert-success' }, [
      m('div', { class: 'flex flex-col gap-1 items-start' }, [
        m('span', { class: 'text-sm font-medium' }, headline),
        m('a', {
          href: '/recordings/' + toast.recordingID,
          class: 'link link-hover text-sm',
          onclick: goToRecording,
        }, 'View recording →'),
      ]),
      m('button', {
        type: 'button',
        class: 'btn btn-xs btn-ghost',
        'aria-label': 'Dismiss',
        onclick: dismiss,
      }, '×'),
    ]),
  ]);
}

export default Queue;
