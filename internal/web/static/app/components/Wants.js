// Wants.js — Mithril port of the legacy /static/wants.js.
//
// The Wants page is the user's Encora "shopping list" — recordings
// flagged on Encora but not yet in collection. Every row's status is
// `wanted`, so there's no filter-by-status dimension here; the page
// is a flat sortable table fronted by a small header summary.
//
// Visual structure (matches design + Library patterns):
//   - Page header: title + "{N} active wants" sub-text + cosmetic
//     "Sync now" / "+ Add want" stub buttons. Buttons are disabled
//     for now — wiring them up is a later wave's work (POST endpoints
//     don't exist yet for the SPA-driven add flow).
//   - Sortable table with columns: Recording, Date, Master, Added,
//     Status. Header click toggles asc/desc; URL persists ?sort=&dir=
//     so deep links / back-forward keep the view.
//
// Default sort: wants_added desc — newest additions surface first,
// which is what the legacy page achieved (it sorted by date_full but
// users actually want recency-of-add, hence the rename of the
// underlying column from last_synced_at to wants_added).

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate, relativeTime } from '../utils/format.js';

// SORT_COLUMNS lists every sortable column in render order. `key` is
// the URL token; `compare` is a stable comparator that sorts in
// ascending order — sortItems below applies the direction sign.
const SORT_COLUMNS = [
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
  { key: 'wants_added',  label: 'Added',
    // Empty wants_added pins to the bottom in either direction —
    // backfilled legacy rows have null and shouldn't dominate the top
    // of an asc sort.
    compare: (a, b) => cmpEmptyLast(a.wants_added, b.wants_added) },
  { key: 'status',       label: 'Status',
    compare: () => 0 }, // every row is `wanted`; sort is a no-op.
];

const DEFAULT_SORT = { key: 'wants_added', dir: 'desc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}
function cmpNum(a, b) { return (Number(a) || 0) - (Number(b) || 0); }
function cmpEmptyLast(a, b) {
  const ea = a == null || a === '';
  const eb = b == null || b === '';
  if (ea && !eb) return 1;
  if (!ea && eb) return -1;
  if (ea && eb) return 0;
  return cmpStr(a, b);
}

// sortItems returns a new sorted slice. wants_added uses cmpEmptyLast
// directly (sign-independent) so empty rows always trail regardless
// of the active direction — same trick Library uses for local_format.
function sortItems(items, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => {
    if (col.key === 'wants_added') return col.compare(a, b);
    return sign * col.compare(a, b);
  });
  return out;
}

// readURLParams pulls the sort selection out of the current Mithril
// route. Wants has no status filter, so this is sort/dir only.
function readURLParams() {
  const params = m.route.param() || {};
  const w = state.wants;

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const col = SORT_COLUMNS.find((c) => c.key === sortKey);
  if (!col) {
    w.sortKey = DEFAULT_SORT.key;
    w.sortDir = DEFAULT_SORT.dir;
  } else {
    w.sortKey = col.key;
    w.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }
}

// pushURLParams syncs state.wants back to the browser URL so a
// reload or share keeps the view.
function pushURLParams() {
  const w = state.wants;
  m.route.set('/wants', {
    sort: w.sortKey,
    dir:  w.sortDir,
  }, { replace: true });
}

// setSort toggles direction when the user clicks the active column,
// otherwise drops to the column's default direction (desc for date /
// wants_added, asc for everything else, mirroring Library's behavior).
function setSort(key) {
  const w = state.wants;
  if (w.sortKey === key) {
    w.sortDir = w.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    w.sortKey = key;
    w.sortDir = (key === 'date' || key === 'wants_added') ? 'desc' : 'asc';
  }
  pushURLParams();
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

// Row renders one wants entry. Click navigates to the recording
// detail page. The status cell is always `Wanted` (badge-info per the
// design doc's status taxonomy).
function Row(it) {
  const subtitle = (it.tour ? it.tour + ' · ' : '') + 'enc-' + it.id;
  const added = it.wants_added ? relativeTime(it.wants_added) : '—';
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + it.id),
  }, [
    m('td', [
      m('div', { class: 'font-medium' }, it.show || '—'),
      m('div', { class: 'text-xs opacity-60' }, subtitle),
    ]),
    m('td', { class: 'font-mono text-sm' },
      smartDate(it.date_full, it.date_month_known, it.date_day_known)),
    m('td', it.master || '—'),
    m('td', {
      class: 'font-mono text-sm',
      title: it.wants_added || '',
    }, added),
    m('td', m('span', { class: 'badge badge-info' }, 'Wanted')),
  ]);
}

const Wants = {
  oninit() {
    readURLParams();
    state.wants.loading = true;
    state.wants.error = null;
    api.get('/wants').then((body) => {
      state.wants.items = (body && body.items) || [];
      state.wants.loading = false;
    }).catch((err) => {
      state.wants.error = err;
      state.wants.loading = false;
    });
  },

  // onupdate fires on every route change while we stay mounted.
  // Wants is only mounted at /wants so any update implies the
  // querystring (sort / dir) may have changed.
  onupdate() {
    readURLParams();
  },

  view() {
    const w = state.wants;
    const items = w.items;

    if (w.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading wants…');
    }
    if (w.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load wants: ' + (w.error.message || w.error)),
      ]);
    }

    const sorted = sortItems(items, { key: w.sortKey, dir: w.sortDir });
    const total = items.length;
    const subText = 'Your shopping list · ' + total +
      (total === 1 ? ' active want' : ' active wants');

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Wants'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        // Action buttons — cosmetic stubs until later waves wire them.
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', {
            type: 'button', class: 'btn btn-ghost btn-sm', disabled: true,
            title: 'Use the CLI for now: promptbook collection sync',
          }, 'Sync now'),
          m('button', {
            type: 'button', class: 'btn btn-primary btn-sm', disabled: true,
            title: 'Use the Encora UI for now',
          }, '+ Add want'),
        ]),
      ]),

      // Empty state — keeps the visual rhythm even with zero rows.
      total === 0
        ? m('div', { class: 'rounded-box bg-base-200 p-8 text-center opacity-70' },
            'No wants on Encora. Add some via the Encora UI and run ' +
            'promptbook collection sync.')
        : m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
            m('table', { class: 'table table-zebra' }, [
              m('thead', m('tr',
                SORT_COLUMNS.map((col) => HeaderCell(col, w.sortKey, w.sortDir)),
              )),
              m('tbody', sorted.map(Row)),
            ])),
    ]);
  },
};

export default Wants;
