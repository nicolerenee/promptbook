// ShowsList.js — the catalog's by-show list page (/shows).
//
// Companion to Recordings.js. Aggregates recordings per show and
// renders a sortable + paginated table OR poster-grid view, with the
// state-counts badge cluster on the FAR RIGHT of the table (states
// is the equivalent of "Status" on the recordings page; both pages
// keep that axis at the right edge for consistency).
//
// View dimension: 'list' | 'grid'. Persisted in localStorage so the
// pref survives navigation; URL ?view= overrides for deep links.
// Sort + page are URL-driven (deep links work).

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import Pagination from './Pagination.js';

const LS_VIEW = 'pb.shows.view';

function readStoredView() {
  try {
    const v = window.localStorage.getItem(LS_VIEW);
    return v === 'grid' || v === 'list' ? v : '';
  } catch (_) {
    return '';
  }
}
function writeStoredView(v) {
  try { window.localStorage.setItem(LS_VIEW, v); } catch (_) { /* no-op */ }
}

// STATUS_META mirrors Recordings.js so the badge cluster colors stay
// consistent across the SPA.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// STATUS_ORDER drives stateBadgeCluster's render sequence so the
// badges always appear in the same visual rhythm.
const STATUS_ORDER = ['synced', 'format_mismatch', 'missing', 'wanted', 'orphan'];

const SORT_COLUMNS = [
  { key: 'name',            label: 'Show' },
  { key: 'recording_count', label: 'Recordings' },
  { key: 'first_year',      label: 'First' },
  { key: 'last_year',       label: 'Last' },
];

const DEFAULT_SORT = { key: 'name', dir: 'asc' };

function defaultDirForKey(key) {
  if (key === 'last_year' || key === 'recording_count') return 'desc';
  return 'asc';
}

function readURLParams() {
  const params = m.route.param() || {};
  const s = state.showsList;

  const rawView = (params.view || '').toLowerCase();
  if (rawView === 'grid' || rawView === 'list') {
    s.view = rawView;
  } else {
    s.view = readStoredView() || 'list';
  }

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const col = SORT_COLUMNS.find((c) => c.key === sortKey);
  if (!col) {
    s.sortKey = DEFAULT_SORT.key;
    s.sortDir = DEFAULT_SORT.dir;
  } else {
    s.sortKey = col.key;
    s.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }

  const page = parseInt(params.page, 10);
  s.offset = (page > 1) ? (page - 1) * s.limit : 0;
}

function pushURLParams() {
  const s = state.showsList;
  const out = {};
  if (s.view !== 'list') out.view = s.view;
  out.sort = s.sortKey;
  out.dir = s.sortDir;
  const page = Math.floor(s.offset / s.limit) + 1;
  if (page > 1) out.page = String(page);
  m.route.set('/shows', out, { replace: true });
}

function loadShows() {
  const s = state.showsList;
  s.loading = true;
  s.error = null;
  const params = new URLSearchParams();
  params.set('limit', String(s.limit));
  params.set('offset', String(s.offset));
  params.set('sort', s.sortKey);
  params.set('dir', s.sortDir);
  return api.get('/shows?' + params.toString()).then((body) => {
    s.items = (body && body.items) || [];
    s.total = (body && body.total) || 0;
    s.loading = false;
  }).catch((err) => {
    s.error = err;
    s.loading = false;
  });
}

function setView(key) {
  if (key !== 'list' && key !== 'grid') return;
  state.showsList.view = key;
  writeStoredView(key);
  pushURLParams();
}

function setSort(key) {
  const s = state.showsList;
  if (s.sortKey === key) {
    s.sortDir = s.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    s.sortKey = key;
    s.sortDir = defaultDirForKey(key);
  }
  s.offset = 0;
  pushURLParams();
  loadShows();
}

function setOffset(newOffset) {
  state.showsList.offset = newOffset;
  pushURLParams();
  loadShows();
}

function MetricTile(label, num, sub) {
  return m('div', { class: 'stat' }, [
    m('div', { class: 'stat-title' }, label),
    m('div', { class: 'stat-value text-2xl' }, String(num)),
    m('div', { class: 'stat-desc' }, sub),
  ]);
}

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

// stateBadgeCluster renders a tight row of small status pills, one
// per non-zero state bucket. Used in the by-show list (states column)
// and the grid card so the reader sees at-a-glance what's covered for
// a show.
function stateBadgeCluster(stateCounts) {
  if (!stateCounts) return [];
  const out = [];
  STATUS_ORDER.forEach((k) => {
    const n = stateCounts[k] || 0;
    if (n === 0) return;
    const meta = STATUS_META[k] || STATUS_META.orphan;
    out.push(m('span', {
      class: 'badge badge-xs ' + meta.badge,
      title: meta.label + ': ' + n,
    }, n));
  });
  return out;
}

function yearSpan(first, last) {
  if (first == null && last == null) return '—';
  if (first == null) return String(last);
  if (last == null) return String(first);
  if (first === last) return String(first);
  return first + '–' + last;
}

// Row renders one show. Column order: Show / Recordings / First /
// Last / States. States is the rightmost cell — same convention as
// Recordings.js where Status is rightmost.
function Row(s) {
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/shows/' + s.id),
  }, [
    m('td', m('div', { class: 'font-medium' }, s.name || '—')),
    m('td', { class: 'font-mono text-sm' }, String(s.recording_count || 0)),
    m('td', { class: 'font-mono text-sm' },
      s.first_year == null ? '—' : String(s.first_year)),
    m('td', { class: 'font-mono text-sm' },
      s.last_year == null ? '—' : String(s.last_year)),
    m('td', m('div', { class: 'flex flex-wrap gap-1' },
      stateBadgeCluster(s.state_counts))),
  ]);
}

function ShowCard(show) {
  const poster = show.local_poster_url || '';
  const onclick = () => m.route.set('/shows/' + show.id);
  const placeholder = m('div', {
    class: 'aspect-[2/3] w-full bg-base-300 flex items-center justify-center text-xs opacity-60 px-2 text-center',
  }, m('span', show.name || '—'));
  const image = m('img', {
    src: poster,
    alt: show.name || '',
    loading: 'lazy',
    class: 'aspect-[2/3] w-full object-cover',
  });
  return m('div', {
    class: 'card bg-base-200 shadow-sm hover:shadow-md hover:ring-1 hover:ring-primary cursor-pointer transition-shadow',
    onclick,
    role: 'button',
    tabindex: 0,
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); onclick(); }
    },
  }, [
    m('div', { class: 'overflow-hidden rounded-t-box w-full' },
      poster ? image : placeholder,
    ),
    m('div', { class: 'card-body p-2 gap-1' }, [
      m('div', { class: 'text-sm font-medium truncate', title: show.name || '' },
        show.name || '—'),
      m('div', { class: 'text-xs opacity-60 font-mono truncate' },
        (show.recording_count || 0) + ' recording' +
          ((show.recording_count === 1) ? '' : 's') +
        ' · ' + yearSpan(show.first_year, show.last_year)),
      m('div', { class: 'flex flex-wrap gap-1' },
        stateBadgeCluster(show.state_counts)),
    ]),
  ]);
}

function IconList() {
  return m('svg', {
    width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none',
    stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', 'aria-hidden': 'true',
  }, [
    m('line', { x1: 8, y1: 6, x2: 21, y2: 6 }),
    m('line', { x1: 8, y1: 12, x2: 21, y2: 12 }),
    m('line', { x1: 8, y1: 18, x2: 21, y2: 18 }),
    m('line', { x1: 3, y1: 6, x2: 3.01, y2: 6 }),
    m('line', { x1: 3, y1: 12, x2: 3.01, y2: 12 }),
    m('line', { x1: 3, y1: 18, x2: 3.01, y2: 18 }),
  ]);
}
function IconGrid() {
  return m('svg', {
    width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none',
    stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', 'aria-hidden': 'true',
  }, [
    m('rect', { x: 3, y: 3, width: 7, height: 7 }),
    m('rect', { x: 14, y: 3, width: 7, height: 7 }),
    m('rect', { x: 3, y: 14, width: 7, height: 7 }),
    m('rect', { x: 14, y: 14, width: 7, height: 7 }),
  ]);
}

function ViewToggle(view) {
  return m('div', { class: 'join', role: 'group', 'aria-label': 'View mode' }, [
    m('button', {
      type: 'button',
      class: 'btn btn-sm join-item' + (view === 'list' ? ' btn-active' : ''),
      'aria-pressed': view === 'list',
      title: 'List view',
      onclick: () => setView('list'),
    }, IconList()),
    m('button', {
      type: 'button',
      class: 'btn btn-sm join-item' + (view === 'grid' ? ' btn-active' : ''),
      'aria-pressed': view === 'grid',
      title: 'Grid view',
      onclick: () => setView('grid'),
    }, IconGrid()),
  ]);
}

function renderList(items, sortKey, sortDir) {
  return m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
    m('table', { class: 'table table-zebra' }, [
      m('thead', m('tr', [
        ...SORT_COLUMNS.map((col) => HeaderCell(col, sortKey, sortDir)),
        m('th', 'States'),
      ])),
      m('tbody', items.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS.length + 1,
            class: 'text-center opacity-60 py-8',
          }, 'No shows in the catalog yet.'))
        : items.map(Row)),
    ]));
}

function renderGrid(items) {
  if (items.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No shows in the catalog yet.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, items.map(ShowCard));
}

const ShowsList = {
  oninit() {
    readURLParams();
    loadShows();
  },

  onupdate() {
    const s = state.showsList;
    const before = {
      sortKey: s.sortKey,
      sortDir: s.sortDir,
      offset: s.offset,
    };
    readURLParams();
    if (before.sortKey !== s.sortKey ||
        before.sortDir !== s.sortDir ||
        before.offset !== s.offset) {
      loadShows();
    }
  },

  view() {
    const s = state.showsList;

    if (s.loading && s.items.length === 0) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading shows…');
    }
    if (s.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' },
        m('span', 'Failed to load shows: ' + (s.error.message || s.error)));
    }

    const total = s.total;
    const offset = s.offset;
    const start = total === 0 ? 0 : offset + 1;
    const end = Math.min(offset + s.limit, total);
    const headerSub = total === 0
      ? 'No shows loaded'
      : 'Showing ' + start + '–' + end + ' of ' + total + ' shows';

    const body = s.view === 'grid'
      ? renderGrid(s.items)
      : renderList(s.items, s.sortKey, s.sortDir);

    return m('div', { class: 'space-y-6' }, [
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Shows'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, headerSub),
        ]),
        m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
          ViewToggle(s.view),
        ]),
      ]),

      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Shows', total, 'in catalog'),
        MetricTile('Page size', s.limit, '50/page default'),
        MetricTile('Page', Math.floor(offset / s.limit) + 1,
          'of ' + Math.max(1, Math.ceil(total / s.limit))),
      ]),

      body,

      m(Pagination, {
        offset,
        limit: s.limit,
        total,
        setOffset,
      }),
    ]);
  },
};

export default ShowsList;
