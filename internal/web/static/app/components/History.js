// History.js — Mithril port of the legacy /static/history.js.
//
// Renders the audit log at /api/v1/history. Tabs filter by `kind`; an
// optional ?recording_id= scopes the list to a single recording's
// timeline. URL state (?kind=, ?recording_id=, ?page=) survives reload.
//
// Visual structure mirrors Library.js: page header w/ sub-text, kind
// filter tabs (tabs-box), then a table. Each row's `details` JSON is
// flattened to a one-line summary instead of the legacy expandable
// pretty-printed pane — the audit pages we hit so far don't lean on the
// raw JSON, and the SPA prefers a compact row.
//
// Pagination + filter both run server-side now: flipping a kind tab
// triggers a refetch (with offset=0) so the totals + page indicator
// stay honest. The shared Pagination component drives Prev / Next.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { relativeTime, errorMessage } from '../utils/format.js';
import Pagination from './Pagination.js';

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

// readURLParams pulls the active kind / recording_id / page filter
// out of the current Mithril route. Mithril gives us m.route.param()
// for the querystring portion.
function readURLParams() {
  const params = m.route.param() || {};
  const h = state.history;

  const rawKind = (params.kind || '').toLowerCase();
  const canon = KIND_FILTERS.find((f) => f.key === rawKind);
  h.kind = canon ? canon.key : '';

  const rawRec = params.recording_id || params.recordingId || '';
  const id = parseInt(rawRec, 10);
  h.recordingID = (id > 0) ? id : null;

  const page = parseInt(params.page, 10);
  h.offset = (page > 1) ? (page - 1) * h.limit : 0;
}

// pushURLParams syncs state.history back to the browser URL so a
// reload or share keeps the same view. Empty params are dropped.
function pushURLParams() {
  const h = state.history;
  const out = {};
  if (h.kind) out.kind = h.kind;
  if (h.recordingID) out.recording_id = String(h.recordingID);
  const page = Math.floor(h.offset / h.limit) + 1;
  if (page > 1) out.page = String(page);
  m.route.set('/history', out, { replace: true });
}

// loadHistory fetches the audit log with the active filters + page.
// Kind filter is applied server-side now (the legacy "fetch 500 and
// filter client-side" trick doesn't survive pagination — total counts
// per tab would be wrong).
function loadHistory() {
  const h = state.history;
  h.loading = true;
  h.error = null;
  const params = new URLSearchParams();
  params.set('limit', String(h.limit));
  params.set('offset', String(h.offset));
  if (h.kind) params.set('kind', h.kind);
  if (h.recordingID) params.set('recording_id', String(h.recordingID));
  return api.get('/history?' + params.toString()).then((body) => {
    h.items = (body && body.items) || [];
    h.total = (body && body.total) || 0;
    h.loading = false;
  }).catch((err) => {
    h.error = err;
    h.loading = false;
  });
}

// setKind updates the active filter, resets offset to 0 (the new
// dataset's row counts may shrink the page count below the current
// page), and refetches.
function setKind(key) {
  const h = state.history;
  h.kind = key;
  h.offset = 0;
  pushURLParams();
  loadHistory();
}

// setOffset is the Pagination component's callback.
function setOffset(newOffset) {
  state.history.offset = newOffset;
  pushURLParams();
  loadHistory();
}

// Tab renders one filter chip. Mirrors Library.js's pattern so the two
// pages share a visual grammar. The count comes off the server's
// total when the tab matches the active filter, otherwise we omit it
// since a per-tab count would require N+1 queries.
function Tab(filter, active, total) {
  const isActive = filter.key === active;
  return m('a', {
    role: 'tab',
    class: 'tab' + (isActive ? ' tab-active' : ''),
    'aria-current': isActive ? 'page' : undefined,
    onclick: (ev) => { ev.preventDefault(); setKind(filter.key); },
    href: '#',
  }, [
    m('span', filter.label),
    isActive
      ? m('span', { class: 'badge badge-sm badge-ghost ml-2' }, total)
      : null,
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
  // querystring (kind / recording_id / page) may have changed.
  onupdate() {
    const prev = {
      kind: state.history.kind,
      rec:  state.history.recordingID,
      off:  state.history.offset,
    };
    readURLParams();
    if (state.history.kind !== prev.kind ||
        state.history.recordingID !== prev.rec ||
        state.history.offset !== prev.off) {
      loadHistory();
    }
  },

  view() {
    const h = state.history;
    const items = h.items;

    if (h.loading && items.length === 0) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading history…');
    }
    if (h.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load history: ' + errorMessage(h.error)),
      ]);
    }

    const total = h.total;
    const start = total === 0 ? 0 : h.offset + 1;
    const end = Math.min(h.offset + h.limit, total);

    const subParts = [];
    if (total === 0) {
      subParts.push('0 events');
    } else {
      subParts.push('Showing ' + start + '–' + end + ' of ' + total +
        ' event' + (total === 1 ? '' : 's'));
    }
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
        KIND_FILTERS.map((f) => Tab(f, h.kind, total))),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table table-zebra' }, [
          m('thead', m('tr', [
            m('th', { style: 'width:140px' }, 'When'),
            m('th', { style: 'width:160px' }, 'Kind'),
            m('th', { style: 'width:140px' }, 'Recording'),
            m('th', 'Details'),
          ])),
          m('tbody', items.length === 0
            ? m('tr', m('td', {
                colspan: 4, class: 'text-center opacity-60 py-8',
              }, total === 0 && !h.kind && !h.recordingID
                ? [
                    'No events yet. Run ',
                    m('code', 'promptbook collection sync'),
                    ' or ingest some recordings to populate the log.',
                  ]
                : 'No events match this filter.'))
            : items.map(Row)),
        ])),

      // Pagination strip — events read better as a "showing X-Y of Z"
      // range than as page numbers because users think in events not
      // pages.
      m(Pagination, {
        offset: h.offset,
        limit: h.limit,
        total: h.total,
        setOffset,
        showRange: true,
      }),
    ]);
  },
};

export default History;
