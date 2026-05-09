// Library.js — Mithril port of the legacy /static/library.js.
//
// Keeps the visual structure: page header w/ sub-text + action buttons,
// 4 metric tiles, status filter tabs, sortable table. Status taxonomy
// is the LOWERCASE storage.Status set ('synced', 'format_mismatch',
// 'missing', 'wanted', 'orphan'). URL params persist the filter / sort
// state via m.route.set so deep links keep working.
//
// Default sort: date desc. local_format pins empty values to the
// bottom on both directions (cmpEmptyLast in legacy library.js).

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate } from '../utils/format.js';

// STATUS_META keys on the lowercase API tokens so meta lookups against
// /api/v1/recordings JSON resolve directly. The DaisyUI badge color
// modifier is intentionally chosen per the design doc:
//   synced          → success
//   format_mismatch → warning
//   missing         → error
//   wanted          → info
//   orphan          → neutral
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// STATUS_FILTERS lists the tabs in display order. Empty key = All.
const STATUS_FILTERS = [
  { key: '',                label: 'All' },
  { key: 'synced',          label: 'Synced' },
  { key: 'format_mismatch', label: 'Format mismatch' },
  { key: 'missing',         label: 'Missing' },
  { key: 'wanted',          label: 'Wanted' },
  { key: 'orphan',          label: 'Orphan' },
];

// SORT_COLUMNS lists every sortable column in render order. `key` is
// the URL token; `compare` is a stable comparator.
const SORT_COLUMNS = [
  { key: 'status',       label: 'Status',
    compare: (a, b) => cmpStr(a.status, b.status) },
  { key: 'recording',    label: 'Recording',
    compare: (a, b) => {
      let c = cmpStr(a.show, b.show); if (c) return c;
      c = cmpStr(a.tour, b.tour); if (c) return c;
      return cmpNum(a.id, b.id);
    } },
  { key: 'date',         label: 'Date',
    compare: (a, b) => cmpStr(a.date_full, b.date_full) },
  { key: 'master',       label: 'Master',
    compare: (a, b) => cmpStr(a.master, b.master) },
  { key: 'local_format', label: 'Local format',
    compare: (a, b) => cmpEmptyLast(a.local_format, b.local_format) },
];

const DEFAULT_SORT = { key: 'date', dir: 'desc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}
function cmpNum(a, b) { return (Number(a) || 0) - (Number(b) || 0); }
// Empty/null values pin to the bottom regardless of direction so
// unmatched local_format rows don't dominate the top of an asc sort.
function cmpEmptyLast(a, b) {
  const ea = a == null || a === '';
  const eb = b == null || b === '';
  if (ea && !eb) return 1;
  if (!ea && eb) return -1;
  if (ea && eb) return 0;
  return cmpStr(a, b);
}

// countByStatus tallies how many items fall into each status. The ''
// key holds the total so the All tab gets a count without a special
// case.
function countByStatus(items) {
  const out = { '': items.length };
  STATUS_FILTERS.forEach((f) => { if (f.key) out[f.key] = 0; });
  items.forEach((it) => { if (out[it.status] != null) out[it.status]++; });
  return out;
}

// fileTotal sums the file_count column so the header sub-text and the
// "Files on disk" tile agree on the number.
function fileTotal(items) {
  return items.reduce((n, it) => n + (it.file_count || 0), 0);
}

// sortItems returns a new sorted slice. local_format uses cmpEmptyLast
// directly (sign-independent) so empty rows always trail.
function sortItems(items, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => {
    if (col.key === 'local_format') return col.compare(a, b);
    return sign * col.compare(a, b);
  });
  return out;
}

// readURLParams pulls the active status / sort state out of the
// current Mithril route. Mithril gives us m.route.param for the
// querystring portion when parsed by the router config below.
function readURLParams() {
  const params = m.route.param() || {};
  const lib = state.library;

  const rawStatus = (params.status || '').toLowerCase();
  const canon = STATUS_FILTERS.find((f) => f.key.toLowerCase() === rawStatus);
  lib.status = canon ? canon.key : '';

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const col = SORT_COLUMNS.find((c) => c.key === sortKey);
  if (!col) {
    lib.sortKey = DEFAULT_SORT.key;
    lib.sortDir = DEFAULT_SORT.dir;
  } else {
    lib.sortKey = col.key;
    lib.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }
}

// pushURLParams syncs state.library back to the browser URL so a
// reload or share keeps the same view. Empty params are dropped.
function pushURLParams() {
  const lib = state.library;
  const out = {};
  if (lib.status) out.status = lib.status;
  out.sort = lib.sortKey;
  out.dir = lib.sortDir;
  m.route.set('/', out, { replace: true });
}

// setStatus updates the active filter and re-syncs the URL.
function setStatus(key) {
  state.library.status = key;
  pushURLParams();
}

// setSort toggles direction when the user clicks the active column,
// otherwise drops to that column's default direction (desc for date,
// asc for everything else, mirroring legacy library.js).
function setSort(key) {
  const lib = state.library;
  if (lib.sortKey === key) {
    lib.sortDir = lib.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    lib.sortKey = key;
    lib.sortDir = key === 'date' ? 'desc' : 'asc';
  }
  pushURLParams();
}

// MetricTile renders one DaisyUI `stat` block. The four tiles share a
// `stats` parent so they line up horizontally on lg+, vertically on
// small screens.
function MetricTile(label, num, sub) {
  return m('div', { class: 'stat' }, [
    m('div', { class: 'stat-title' }, label),
    m('div', { class: 'stat-value text-2xl' }, String(num)),
    m('div', { class: 'stat-desc' }, sub),
  ]);
}

// Tab renders a single status filter chip. DaisyUI's tabs-box style is
// the "pill row" we want; tab-active marks the chosen one.
function Tab(filter, counts, active) {
  const isActive = filter.key === active;
  return m('a', {
    role: 'tab',
    class: 'tab' + (isActive ? ' tab-active' : ''),
    'aria-current': isActive ? 'page' : undefined,
    onclick: (ev) => { ev.preventDefault(); setStatus(filter.key); },
    href: '#',
  }, [
    m('span', filter.label),
    m('span', {
      class: 'badge badge-sm badge-ghost ml-2',
    }, counts[filter.key] || 0),
  ]);
}

// HeaderCell renders one sortable <th>. Caret indicates the active
// direction; aria-sort matches for screen readers.
function HeaderCell(col, sortKey, sortDir) {
  const isActive = col.key === sortKey;
  let caret = '';
  if (isActive) caret = sortDir === 'desc' ? ' ▼' : ' ▲';
  const ariaSort = isActive
    ? (sortDir === 'desc' ? 'descending' : 'ascending')
    : 'none';
  return m('th', {
    class: 'cursor-pointer select-none',
    'aria-sort': ariaSort,
    tabindex: 0,
    role: 'button',
    onclick: () => setSort(col.key),
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') {
        ev.preventDefault();
        setSort(col.key);
      }
    },
  }, col.label + caret);
}

// Row renders one recording. Click navigates to the (stub for now)
// detail page via the SPA router.
function Row(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const subtitle = (it.tour ? it.tour + ' · ' : '') + 'enc-' + it.id;
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + it.id),
  }, [
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
    m('td', [
      m('div', { class: 'font-medium' }, it.show || '—'),
      m('div', { class: 'text-xs opacity-60' }, subtitle),
    ]),
    m('td', { class: 'font-mono text-sm' },
      smartDate(it.date_full, it.date_month_known, it.date_day_known)),
    m('td', it.master || '—'),
    m('td', { class: 'font-mono text-sm' }, it.local_format || '—'),
  ]);
}

const Library = {
  oninit() {
    readURLParams();
    state.library.loading = true;
    state.library.error = null;
    api.get('/recordings?limit=200').then((body) => {
      state.library.items = (body && body.items) || [];
      state.library.loading = false;
    }).catch((err) => {
      state.library.error = err;
      state.library.loading = false;
    });
  },

  // onupdate fires on every route change while we stay mounted. The
  // SPA uses Library for `/` only, so any update implies the
  // querystring (status / sort / dir) may have changed. Re-read,
  // re-render.
  onupdate() {
    readURLParams();
  },

  view() {
    const lib = state.library;
    const items = lib.items;
    const counts = countByStatus(items);
    const total = items.length;
    const synced = counts.synced || 0;
    const wanted = counts.wanted || 0;
    const mismatchTotal =
      (counts.format_mismatch || 0) +
      (counts.missing || 0) +
      (counts.orphan || 0);
    const files = fileTotal(items);

    if (lib.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading library…');
    }
    if (lib.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load recordings: ' + (lib.error.message || lib.error)),
      ]);
    }

    const filtered = lib.status
      ? items.filter((it) => it.status === lib.status)
      : items.slice();
    const sorted = sortItems(filtered, { key: lib.sortKey, dir: lib.sortDir });

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Library'),
          m('p', { class: 'text-sm opacity-70 mt-1' },
            total + ' cataloged · ' + synced + ' synced · ' +
            wanted + ' wanted · ' + files + ' files on disk'),
        ]),
        // Action buttons — cosmetic stubs until later waves wire them.
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', { type: 'button', class: 'btn btn-ghost btn-sm', disabled: true }, 'Filters'),
          m('button', { type: 'button', class: 'btn btn-sm', disabled: true }, 'Sync now'),
          m('button', { type: 'button', class: 'btn btn-primary btn-sm', disabled: true }, 'Manual import'),
        ]),
      ]),

      // Stat tiles. `stats` parent wraps `stat` children per DaisyUI.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Synced',     synced,         total + ' cataloged'),
        MetricTile('Wanted',     wanted,         'on the shopping list'),
        MetricTile('Mismatches', mismatchTotal,  'needs reconcile'),
        MetricTile('Files on disk', files,       'across all versions'),
      ]),

      // Status filter tabs.
      m('div', { role: 'tablist', class: 'tabs tabs-box' },
        STATUS_FILTERS.map((f) => Tab(f, counts, lib.status))),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table table-zebra' }, [
          m('thead', m('tr',
            SORT_COLUMNS.map((col) => HeaderCell(col, lib.sortKey, lib.sortDir)),
          )),
          m('tbody', sorted.length === 0
            ? m('tr', m('td', {
                colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
              }, 'No recordings match this filter.'))
            : sorted.map(Row)),
        ])),
    ]);
  },
};

export default Library;
