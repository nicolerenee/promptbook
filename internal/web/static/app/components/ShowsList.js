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
import graphql from '../graphql.js';
import state from '../state.js';

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
  out_of_sync: { label: 'Out of sync', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// STATUS_ORDER drives stateBadgeCluster's render sequence so the
// badges always appear in the same visual rhythm.
const STATUS_ORDER = ['synced', 'out_of_sync', 'missing', 'wanted', 'orphan'];

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

  // Pagination is gone from the UI; offset stays at 0 forever.
  s.offset = 0;
}

function pushURLParams() {
  const s = state.showsList;
  const out = {};
  if (s.view !== 'list') out.view = s.view;
  out.sort = s.sortKey;
  out.dir = s.sortDir;
  m.route.set('/shows', out, { replace: true });
}

// CATALOG_LIMIT — see Recordings.js. Single-fetch-everything for the
// shows list page.
const CATALOG_LIMIT = 100_000;

// SHOWS_LIST_QUERY hits the showsList custom resolver — the offset-
// paginated by-show aggregate that mirrors /api/v1/shows. The
// stateCounts subselection picks the per-status fields the SPA renders
// as a badge cluster.
const SHOWS_LIST_QUERY = `
  query ShowsList($sort: String, $dir: String, $limit: Int, $offset: Int) {
    showsList(sort: $sort, dir: $dir, limit: $limit, offset: $offset) {
      total
      items {
        id
        name
        recordingCount
        firstYear
        lastYear
        localPosterURL
        stateCounts {
          synced
          outOfSync
          missing
          wanted
          orphan
        }
      }
    }
  }
`;

// stripIDPrefix turns "show-1234" into "1234". The /shows/:id routes
// take int64 ids so the SPA strips the prefix at the GraphQL boundary.
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// mapShowItem rewrites a GraphQL ShowsListItem into the snake_case
// shape the renderer expects. state_counts is a flat
// {[token]: count} map post-mapping so the badge cluster keeps its
// existing shape.
function mapShowItem(node) {
  if (!node) return null;
  const sc = node.stateCounts || {};
  return {
    id:               Number(stripIDPrefix(node.id)),
    name:             node.name || '',
    recording_count:  node.recordingCount || 0,
    first_year:       node.firstYear == null ? null : node.firstYear,
    last_year:        node.lastYear == null ? null : node.lastYear,
    local_poster_url: node.localPosterURL || '',
    state_counts: {
      synced:          sc.synced || 0,
      out_of_sync: sc.outOfSync || 0,
      missing:         sc.missing || 0,
      wanted:          sc.wanted || 0,
      orphan:          sc.orphan || 0,
    },
  };
}

// FRESHNESS_MS — see Recordings.js's loadRecordings for the rationale.
// Browser back-navigation lands on this code path; if the cached
// items are still fresh and the query inputs match, skip the round-
// trip so the user keeps scroll position + sees the list instantly.
const FRESHNESS_MS = 30_000;

function loadShows() {
  const s = state.showsList;
  if (s.lastLoadedAt && s.items && s.items.length > 0 &&
      Date.now() - s.lastLoadedAt < FRESHNESS_MS &&
      s.lastQuery === showsQueryKey(s)) {
    return Promise.resolve();
  }
  if (!s.items || s.items.length === 0) {
    s.loading = true;
  }
  s.error = null;
  const variables = {
    limit:  CATALOG_LIMIT,
    offset: 0,
    sort:   s.sortKey,
    dir:    s.sortDir,
  };
  const queryAtFire = showsQueryKey(s);
  return graphql.query(SHOWS_LIST_QUERY, variables).then((data) => {
    if (queryAtFire !== showsQueryKey(s)) return;
    const env = (data && data.showsList) || {};
    const items = Array.isArray(env.items) ? env.items : [];
    s.items = items.map(mapShowItem).filter(Boolean);
    s.total = env.total || 0;
    s.loading = false;
    s.lastLoadedAt = Date.now();
    s.lastQuery = queryAtFire;
  }).catch((err) => {
    s.error = err;
    s.loading = false;
  });
}

function showsQueryKey(s) {
  return [s.sortKey, s.sortDir].join('|');
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
    // Mithril `key` lets the diff reconcile rows by id when the
    // server returns a fresh items array (back-navigation refresh,
    // etc.) — without it every row's DOM node is destroyed and
    // recreated, which kicks fresh poster fetches and resets scroll.
    key: 'show-' + s.id,
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
    // See Row's `key` — same reasoning, doubly important for the
    // grid view where each card fires its own poster fetch.
    key: 'show-' + show.id,
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
    };
    readURLParams();
    if (before.sortKey !== s.sortKey ||
        before.sortDir !== s.sortDir) {
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
    const headerSub = total === 0
      ? 'No shows loaded'
      : String(total) + ' show' + (total === 1 ? '' : 's') + ' in catalog';

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

      body,
    ]);
  },
};

export default ShowsList;
