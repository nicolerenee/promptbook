// Wants.js — Mithril port of the legacy /static/wants.js.
//
// The Wants page is the user's Encora "shopping list" — recordings
// flagged on Encora but not yet in collection. Every row's status is
// `wanted`, so there's no filter-by-status dimension here; the page
// is a flat sortable + paginated table fronted by a small header
// summary.
//
// Visual structure (matches design + Library patterns):
//   - Page header: title + "Showing X–Y of Z" sub-text + cosmetic
//     "Sync now" / "+ Add want" stub buttons. Buttons are disabled
//     for now — wiring them up is a later wave's work (POST endpoints
//     don't exist yet for the SPA-driven add flow).
//   - Sortable + paginated table with columns: Recording, Date,
//     Master, Added, Status. Header click toggles asc/desc; URL
//     persists ?sort=&dir=&page= so deep links / back-forward keep
//     the view.
//
// Default sort: wants_added desc — newest additions surface first,
// which is what the legacy page achieved (it sorted by date_full but
// users actually want recency-of-add, hence the rename of the
// underlying column from last_synced_at to wants_added).

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate, relativeTime } from '../utils/format.js';
import Pagination from './Pagination.js';

// SORT_COLUMNS lists every sortable column in render order. `key` is
// the URL token that the server resolves against its whitelist
// (wantsSortFragments). Status is fixed (every row is wanted) so
// there's no sort key for it.
const SORT_COLUMNS = [
  { key: 'recording',    label: 'Recording' },
  { key: 'date',         label: 'Date' },
  { key: 'master',       label: 'Master' },
  { key: 'wants_added',  label: 'Added' },
];

const DEFAULT_SORT = { key: 'wants_added', dir: 'desc' };

// readURLParams pulls the sort + page selection out of the current
// Mithril route. Wants has no status filter, so this is sort/dir/page
// only.
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

  const page = parseInt(params.page, 10);
  w.offset = (page > 1) ? (page - 1) * w.limit : 0;
}

// pushURLParams syncs state.wants back to the browser URL so a
// reload or share keeps the view. page=1 is dropped since it's the
// default.
function pushURLParams() {
  const w = state.wants;
  const out = {
    sort: w.sortKey,
    dir:  w.sortDir,
  };
  const page = Math.floor(w.offset / w.limit) + 1;
  if (page > 1) out.page = String(page);
  m.route.set('/wants', out, { replace: true });
}

// loadWants fetches the active page from the server.
function loadWants() {
  const w = state.wants;
  w.loading = true;
  w.error = null;
  const params = new URLSearchParams();
  params.set('limit', String(w.limit));
  params.set('offset', String(w.offset));
  params.set('sort', w.sortKey);
  params.set('dir', w.sortDir);
  return api.get('/wants?' + params.toString()).then((body) => {
    w.items = (body && body.items) || [];
    w.total = (body && body.total) || 0;
    w.loading = false;
  }).catch((err) => {
    w.error = err;
    w.loading = false;
  });
}

// setSort toggles direction when the user clicks the active column,
// otherwise drops to the column's default direction (desc for date /
// wants_added, asc for everything else, mirroring Library's behavior).
// Resets to page 1 since the row at offset N changes meaning when the
// sort axis flips.
function setSort(key) {
  const w = state.wants;
  if (w.sortKey === key) {
    w.sortDir = w.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    w.sortKey = key;
    w.sortDir = (key === 'date' || key === 'wants_added') ? 'desc' : 'asc';
  }
  w.offset = 0;
  pushURLParams();
  loadWants();
}

// setOffset is the Pagination component's callback.
function setOffset(newOffset) {
  state.wants.offset = newOffset;
  pushURLParams();
  loadWants();
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
    loadWants();
  },

  // onupdate fires on every route change while we stay mounted.
  // Wants is only mounted at /wants so any update implies the
  // querystring (sort / dir / page) may have changed.
  onupdate() {
    readURLParams();
  },

  view() {
    const w = state.wants;

    if (w.loading && w.items.length === 0) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading wants…');
    }
    if (w.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load wants: ' + (w.error.message || w.error)),
      ]);
    }

    const items = w.items;
    const total = w.total;
    const start = total === 0 ? 0 : w.offset + 1;
    const end = Math.min(w.offset + w.limit, total);
    const subText = total === 0
      ? 'Your shopping list · 0 active wants'
      : 'Your shopping list · Showing ' + start + '–' + end + ' of ' + total +
        ' active want' + (total === 1 ? '' : 's');

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
              m('thead', m('tr', [
                ...SORT_COLUMNS.map((col) => HeaderCell(col, w.sortKey, w.sortDir)),
                m('th', 'Status'),
              ])),
              m('tbody', items.map(Row)),
            ])),

      // Pagination strip.
      m(Pagination, {
        offset: w.offset,
        limit: w.limit,
        total: w.total,
        setOffset,
      }),
    ]);
  },
};

export default Wants;
