// History.js — Mithril port of the legacy /static/history.js.
//
// Renders the audit log at /api/v1/history. Tabs filter by `kind`; an
// optional ?recording_id= scopes the list to a single recording's
// timeline. URL state (?kind=, ?recording_id=) survives reload.
//
// Visual structure mirrors Library.js: page header w/ sub-text, kind
// filter tabs (tabs-box), then a table. Each row's `details` JSON is
// flattened to a one-line summary instead of the legacy expandable
// pretty-printed pane — the audit pages we hit so far don't lean on the
// raw JSON, and the SPA prefers a compact row.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { relativeTime, errorMessage } from '../utils/format.js';

// KIND_META keys on the lowercase API tokens, matching
// internal/storage/history.go's HistoryKind* constants. Badge colors
// come from the design doc's tone-per-kind table.
const KIND_META = {
  ingest:        { label: 'Ingest',        badge: 'badge-success' },
  rename:        { label: 'Rename',        badge: 'badge-info'    },
  nfo_write:     { label: 'NFO Write',     badge: 'badge-info'    },
  encora_push:   { label: 'Encora Push',   badge: 'badge-warning' },
  sync:          { label: 'Sync',          badge: 'badge-neutral' },
  manual_import: { label: 'Manual Import', badge: 'badge-success' },
};

// KIND_FILTERS lists the tabs in display order. Empty key = All.
const KIND_FILTERS = [
  { key: '',              label: 'All' },
  { key: 'ingest',        label: 'Ingest' },
  { key: 'rename',        label: 'Rename' },
  { key: 'nfo_write',     label: 'NFO' },
  { key: 'encora_push',   label: 'Encora push' },
  { key: 'sync',          label: 'Sync' },
  { key: 'manual_import', label: 'Manual import' },
];

// truncate caps a string at n chars and adds an ellipsis. Used to keep
// summary cells tidy when long file paths sneak into details.
function truncate(s, n) {
  if (s == null) return '';
  const str = String(s);
  if (str.length <= n) return str;
  return str.substring(0, n - 1) + '…';
}

// detailSummary derives a one-line human label from a history event's
// details object. Falls back to summary or the kind label so a row is
// never blank — even for a kind we don't recognize.
function detailSummary(it) {
  const d = it.details || {};
  switch (it.kind) {
    case 'ingest': {
      const enc = d.encora_id || it.recording_id;
      const dest = d.dest || d.destination || d.path || '';
      if (enc && dest) return 'enc-' + enc + ' → ' + truncate(dest, 80);
      if (enc) return 'enc-' + enc;
      break;
    }
    case 'rename': {
      const from = d.from || d.src || d.source || '';
      const to = d.to || d.dest || d.destination || '';
      if (from && to) return truncate(from, 40) + ' → ' + truncate(to, 40);
      if (to) return '→ ' + truncate(to, 80);
      break;
    }
    case 'nfo_write': {
      const path = d.path || d.dest || d.file || '';
      if (path) return truncate(path, 100);
      break;
    }
    case 'encora_push': {
      const action = d.action || 'pushed';
      const enc = d.encora_id || it.recording_id;
      if (enc) return action + ' enc-' + enc;
      return action;
    }
    case 'sync': {
      const added = d.added != null ? d.added : 0;
      const updated = d.updated != null ? d.updated : 0;
      const wants = d.wants_added != null ? d.wants_added : 0;
      const parts = [];
      if (added)   parts.push(added + ' added');
      if (updated) parts.push(updated + ' updated');
      if (wants)   parts.push(wants + ' wants');
      if (parts.length) return parts.join(' · ');
      break;
    }
    case 'manual_import': {
      const enc = d.encora_id || it.recording_id;
      const src = d.source || '';
      if (enc && src) return 'enc-' + enc + ' ← ' + truncate(src, 80);
      if (enc) return 'enc-' + enc;
      break;
    }
    default: break;
  }
  if (it.summary) return it.summary;
  const meta = KIND_META[it.kind];
  return meta ? meta.label : (it.kind || '');
}

// countByKind returns {kind: count, '': total} so each tab can show
// its tally without re-filtering the array.
function countByKind(items) {
  const out = { '': items.length };
  KIND_FILTERS.forEach((f) => { if (f.key) out[f.key] = 0; });
  items.forEach((it) => { if (out[it.kind] != null) out[it.kind]++; });
  return out;
}

// readURLParams pulls the active kind / recording_id filter out of the
// current Mithril route. Mithril gives us m.route.param() for the
// querystring portion.
function readURLParams() {
  const params = m.route.param() || {};
  const h = state.history;

  const rawKind = (params.kind || '').toLowerCase();
  const canon = KIND_FILTERS.find((f) => f.key === rawKind);
  h.kind = canon ? canon.key : '';

  const rawRec = params.recording_id || params.recordingId || '';
  const id = parseInt(rawRec, 10);
  h.recordingID = (id > 0) ? id : null;
}

// pushURLParams syncs state.history back to the browser URL so a
// reload or share keeps the same view. Empty params are dropped.
function pushURLParams() {
  const h = state.history;
  const out = {};
  if (h.kind) out.kind = h.kind;
  if (h.recordingID) out.recording_id = String(h.recordingID);
  m.route.set('/history', out, { replace: true });
}

// loadHistory fetches the audit log with the active recording_id
// filter (if any). The kind filter is applied client-side so changing
// tabs doesn't trigger a refetch — the dataset is small (50-page
// paginated default, 500 here) and counting stays accurate across
// tabs.
function loadHistory() {
  const h = state.history;
  h.loading = true;
  h.error = null;
  let path = '/history?limit=500';
  if (h.recordingID) path += '&recording_id=' + h.recordingID;
  return api.get(path).then((body) => {
    h.items = (body && body.items) || [];
    h.loading = false;
  }).catch((err) => {
    h.error = err;
    h.loading = false;
  });
}

// setKind updates the active filter and re-syncs the URL. No refetch
// — filtering happens client-side off the cached list.
function setKind(key) {
  state.history.kind = key;
  pushURLParams();
}

// Tab renders one filter chip. Mirrors Library.js's pattern so the two
// pages share a visual grammar.
function Tab(filter, counts, active) {
  const isActive = filter.key === active;
  return m('a', {
    role: 'tab',
    class: 'tab' + (isActive ? ' tab-active' : ''),
    'aria-current': isActive ? 'page' : undefined,
    onclick: (ev) => { ev.preventDefault(); setKind(filter.key); },
    href: '#',
  }, [
    m('span', filter.label),
    m('span', { class: 'badge badge-sm badge-ghost ml-2' },
      counts[filter.key] || 0),
  ]);
}

// RecordingCell renders the optional recording_id column. Links to
// /recordings/:id via the SPA router when set.
function RecordingCell(it) {
  if (!it.recording_id) return m('span', { class: 'opacity-60' }, '—');
  const href = '/recordings/' + it.recording_id;
  return m('a', {
    class: 'link link-hover font-mono text-sm',
    href,
    onclick: (ev) => { ev.preventDefault(); m.route.set(href); },
  }, 'enc-' + it.recording_id);
}

// Row renders one audit event.
function Row(it) {
  const meta = KIND_META[it.kind] || { label: it.kind || '—', badge: 'badge-neutral' };
  return m('tr', { key: it.id }, [
    m('td', {
      class: 'font-mono text-sm whitespace-nowrap',
      title: it.occurred_at,
    }, relativeTime(it.occurred_at)),
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
    m('td', RecordingCell(it)),
    m('td', { class: 'text-sm' }, detailSummary(it)),
  ]);
}

const History = {
  oninit() {
    readURLParams();
    loadHistory();
  },

  // onupdate fires on every route change while we stay mounted. The
  // SPA uses History for /history only, so any update implies the
  // querystring (kind / recording_id) may have changed. Re-read; if
  // recording_id changed we need to refetch, otherwise just redraw.
  onupdate() {
    const prevRecording = state.history.recordingID;
    readURLParams();
    if (state.history.recordingID !== prevRecording) {
      loadHistory();
    }
  },

  view() {
    const h = state.history;
    const items = h.items;

    if (h.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading history…');
    }
    if (h.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load history: ' + errorMessage(h.error)),
      ]);
    }

    const counts = countByKind(items);
    const filtered = h.kind
      ? items.filter((it) => it.kind === h.kind)
      : items.slice();

    const subParts = [];
    subParts.push(items.length + ' event' + (items.length === 1 ? '' : 's'));
    if (h.recordingID) subParts.push('scoped to enc-' + h.recordingID);
    subParts.push('newest first');
    const subText = subParts.join(' · ');

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'History'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        // Action stubs — kept disabled until a future wave wires
        // export / date-range filters.
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: true,
            title: '(coming soon)',
          }, 'Date range'),
          m('a', {
            class: 'btn btn-ghost btn-sm',
            href: '/api/v1/history?limit=10000',
            download: 'promptbook-history.json',
          }, 'Export JSON'),
        ]),
      ]),

      // Kind filter tabs.
      m('div', { role: 'tablist', class: 'tabs tabs-box' },
        KIND_FILTERS.map((f) => Tab(f, counts, h.kind))),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table table-zebra' }, [
          m('thead', m('tr', [
            m('th', { style: 'width:140px' }, 'When'),
            m('th', { style: 'width:160px' }, 'Kind'),
            m('th', { style: 'width:140px' }, 'Recording'),
            m('th', 'Details'),
          ])),
          m('tbody', filtered.length === 0
            ? m('tr', m('td', {
                colspan: 4, class: 'text-center opacity-60 py-8',
              }, items.length === 0
                ? [
                    'No events yet. Run ',
                    m('code', 'promptbook collection sync'),
                    ' or ingest some recordings to populate the log.',
                  ]
                : 'No events match this filter.'))
            : filtered.map(Row)),
        ])),
    ]);
  },
};

export default History;
