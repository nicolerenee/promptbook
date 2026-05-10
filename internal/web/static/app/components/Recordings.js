// Recordings.js — the catalog's recording-list page (/).
//
// Page header w/ sub-text + action stubs, status filter chips,
// sortable + paginated table OR poster grid. Status taxonomy is the
// LOWERCASE storage.Status set ('synced', 'format_mismatch',
// 'missing', 'wanted', 'orphan'). Wants are reachable via
// status='wanted'; the dedicated /wants page is gone.
//
// View dimension: 'list' | 'grid'. Persisted in localStorage so the
// pref survives navigation; URL ?view= overrides for deep links.
// Sort + status + page are URL-driven (deep links work); they are
// NOT persisted across navigations.
//
// Status column lives on the FAR RIGHT of the table. Recording is the
// primary identifier so it gets the leftmost slot.
//
// Phase 4b — data fetch is GraphQL (`recordings(first:, after:, …)`),
// not REST. The connection's cursor strings get cached per-page so
// Prev/Next + the offset-style URL ?page= continue to work; jumping
// to an arbitrary page walks forward through cursors when no cache
// hit. status maps to RecordingWhereInput; sort maps to
// RecordingOrder; the reconciler-derived `status` field comes from
// our enrichment resolver (see internal/server/graph/enrichment.graphql).

import m from 'https://esm.sh/mithril@2.2.2';
import graphql from '../graphql.js';
import state from '../state.js';
import { smartDateWithVariant } from '../utils/format.js';

// LS_VIEW is the localStorage key the recordings page uses to persist
// the user's preferred layout across visits. The URL still wins when
// it carries an explicit ?view= (so deep links work); localStorage is
// the fallback when the URL is bare.
const LS_VIEW = 'pb.recordings.view';

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

// STATUS_META keys on the lowercase API tokens so meta lookups
// against /api/v1/recordings JSON resolve directly.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// STATUS_FILTERS lists the chips in display order. Empty key = All.
const STATUS_FILTERS = [
  { key: '',                label: 'All' },
  { key: 'synced',          label: 'Synced' },
  { key: 'format_mismatch', label: 'Format mismatch' },
  { key: 'missing',         label: 'Missing' },
  { key: 'wanted',          label: 'Wanted' },
  { key: 'orphan',          label: 'Orphan' },
];

// SORT_COLUMNS lists the sortable columns in their RENDER order.
// Status is rightmost (least interesting axis to scan; row-identity
// belongs on the left). The "Release format" column shows the
// locally-derived release-format string (see releaseformat.Compose
// on the server) — replaces the legacy "Local format" column that
// pulled the per-version FormatLabel join.
const SORT_COLUMNS = [
  { key: 'recording',    label: 'Recording' },
  { key: 'date',         label: 'Date' },
  { key: 'master',       label: 'Master' },
  { key: 'local_format', label: 'Release format' },
  { key: 'status',       label: 'Status' },
];

const DEFAULT_SORT = { key: 'recording', dir: 'asc' };

function defaultDirForKey(key) {
  if (key === 'date') return 'desc';
  return 'asc';
}

function readURLParams() {
  const params = m.route.param() || {};
  const r = state.recordings;

  const rawView = (params.view || '').toLowerCase();
  if (rawView === 'grid' || rawView === 'list') {
    r.view = rawView;
  } else {
    r.view = readStoredView() || 'list';
  }

  const rawStatus = (params.status || '').toLowerCase();
  const canon = STATUS_FILTERS.find((f) => f.key.toLowerCase() === rawStatus);
  r.status = canon ? canon.key : '';

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const col = SORT_COLUMNS.find((c) => c.key === sortKey);
  if (!col) {
    r.sortKey = DEFAULT_SORT.key;
    r.sortDir = DEFAULT_SORT.dir;
  } else {
    r.sortKey = col.key;
    r.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }

  // Pagination is gone from the UI — list pages always fetch the
  // full catalog in one go now. offset stays at 0 forever; limit
  // gets bumped to a catalog-sized number in loadRecordings.
  r.offset = 0;
}

function pushURLParams() {
  const r = state.recordings;
  const out = {};
  if (r.status) out.status = r.status;
  if (r.view !== 'list') out.view = r.view;
  out.sort = r.sortKey;
  out.dir = r.sortDir;
  m.route.set('/', out, { replace: true });
}

// RECORDINGS_LIST_QUERY hits the recordingsList custom resolver — the
// offset-paginated, status-aware envelope that mirrors the legacy
// /api/v1/recordings shape. Field names are camelCase per gqlgen
// convention; mapRecordingItem rewrites the response to the
// snake_case keys the table + grid renderers were written against.
const RECORDINGS_LIST_QUERY = `
  query RecordingsList($status: String, $sort: String, $dir: String, $limit: Int, $offset: Int) {
    recordingsList(status: $status, sort: $sort, dir: $dir, limit: $limit, offset: $offset) {
      total
      items {
        id
        showID
        show
        tour
        dateFull
        dateMonthKnown
        dateDayKnown
        dateVariant
        master
        status
        inCollection
        inWants
        fileCount
        encoraFormat
        localFormat
        localReleaseFormat
        localPosterURL
      }
    }
  }
`;

// stripIDPrefix turns "recording-1234" into "1234". The SPA's URL
// params still carry bare int64s because the routes
// (/recordings/:id) were never changed; the GraphQL surface produces
// the prefixed form so we strip it at the boundary.
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// mapRecordingItem rewrites a GraphQL RecordingsListItem into the
// snake_case shape the renderer + sort comparators expect. Keeps the
// shape decode-compatible with the legacy REST payload so the
// downstream renderers stay untouched.
function mapRecordingItem(node) {
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
    status:           node.status || '',
    in_collection:    !!node.inCollection,
    in_wants:         !!node.inWants,
    file_count:       node.fileCount || 0,
    encora_format:    node.encoraFormat || '',
    local_format:     node.localFormat || '',
    // local_release_format is the locally-derived "what files do
    // you have" string — see releaseformat.Compose on the server.
    // Replaces the legacy local_format string in the table column;
    // local_format stays in the payload for the format-mismatch
    // sort+filter machinery that still keys off the reconciler's
    // join string.
    local_release_format: node.localReleaseFormat || '',
    local_poster_url: node.localPosterURL || '',
  };
}

// FRESHNESS_MS caps how long a previous load is considered "fresh"
// for the back-navigation skip path. ~30s leaves a noticeable
// latency budget if the user genuinely returns mid-session, but
// short enough that a multi-minute round-trip will refresh on
// re-mount.
const FRESHNESS_MS = 30_000;

// CATALOG_LIMIT is the upper bound the SPA passes to the server's
// recordingsList resolver. Visible pagination is gone — we always
// fetch the entire catalog in a single round-trip. The server caps
// at maxStateScan (100k) which sits well above any realistic
// personal Broadway catalog.
const CATALOG_LIMIT = 100_000;

function loadRecordings() {
  const r = state.recordings;
  // Cached items + same query params + recent fetch → skip the
  // round-trip. Browser back-navigation lands on this path; the
  // user keeps their scroll position + the list renders
  // immediately from already-mapped state.
  if (r.lastLoadedAt && r.items && r.items.length > 0 &&
      Date.now() - r.lastLoadedAt < FRESHNESS_MS &&
      r.lastQuery === queryKey(r)) {
    return Promise.resolve();
  }
  // Don't toggle loading=true when we already have a list to show
  // — the empty-state placeholder only fires on first paint, and
  // a silent in-place refresh with no UI churn is what the user
  // asked for. The view check `loading && items.length === 0`
  // handles this naturally; we just keep the flag scoped to the
  // empty case.
  if (!r.items || r.items.length === 0) {
    r.loading = true;
  }
  r.error = null;
  const variables = {
    // Single-fetch-everything: pagination is gone from the UI, so
    // we ask the server for the full catalog. The server caps at
    // maxStateScan (100k) which is well above any realistic
    // catalog size.
    limit:  CATALOG_LIMIT,
    offset: 0,
    sort:   r.sortKey,
    dir:    r.sortDir,
    status: r.status || null,
  };
  const queryAtFire = queryKey(r);
  return graphql.query(RECORDINGS_LIST_QUERY, variables).then((data) => {
    // Stale-response guard: if the user's filters/page changed
    // mid-flight, drop this response so the UI doesn't flash an
    // older result over the newer query's items.
    if (queryAtFire !== queryKey(r)) return;
    const env = (data && data.recordingsList) || {};
    const items = Array.isArray(env.items) ? env.items : [];
    r.items = items.map(mapRecordingItem).filter(Boolean);
    r.total = env.total || 0;
    r.loading = false;
    r.lastLoadedAt = Date.now();
    r.lastQuery = queryAtFire;
  }).catch((err) => {
    r.error = err;
    r.loading = false;
  });
}

// queryKey is a deterministic string of the inputs that affect
// the result set so the freshness check + the stale-response guard
// can compare with === instead of structural equality.
function queryKey(r) {
  // offset + limit aren't user-tunable anymore (single-fetch-
  // everything), so the cache key only varies on the inputs the
  // user can actually change.
  return [r.status || '', r.sortKey, r.sortDir].join('|');
}

function setStatus(key) {
  const r = state.recordings;
  r.status = key;
  r.offset = 0;
  pushURLParams();
  loadRecordings();
}

function setView(key) {
  if (key !== 'list' && key !== 'grid') return;
  state.recordings.view = key;
  writeStoredView(key);
  pushURLParams();
}

function setSort(key) {
  const r = state.recordings;
  if (r.sortKey === key) {
    r.sortDir = r.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    r.sortKey = key;
    r.sortDir = defaultDirForKey(key);
  }
  r.offset = 0;
  pushURLParams();
  loadRecordings();
}

function Tab(filter, active) {
  const isActive = filter.key === active;
  return m('a', {
    role: 'tab',
    class: 'tab' + (isActive ? ' tab-active' : ''),
    'aria-current': isActive ? 'page' : undefined,
    onclick: (ev) => { ev.preventDefault(); setStatus(filter.key); },
    href: '#',
  }, m('span', filter.label));
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

// Row renders one recording. Column order matches SORT_COLUMNS so
// Recording → Date → Master → Local format → Status. Status is the
// rightmost cell as a small badge.
function Row(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const subtitle = (it.tour ? it.tour + ' · ' : '') + 'enc-' + it.id;
  return m('tr', {
    // Mithril `key` lets the diff reconcile rows by id when the
    // server returns a fresh items array (e.g. on back-navigation
    // refresh) — without it every row's DOM node is destroyed +
    // recreated, which kicks 50 fresh poster fetches and resets
    // the user's scroll position.
    key: 'rec-' + it.id,
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + it.id),
  }, [
    m('td', [
      m('div', { class: 'font-medium' }, it.show || '—'),
      m('div', { class: 'text-xs opacity-60' }, subtitle),
    ]),
    m('td', { class: 'font-mono text-sm' },
      smartDateWithVariant(
        it.date_full, it.date_month_known, it.date_day_known, it.date_variant)),
    m('td', it.master || '—'),
    m('td', { class: 'font-mono text-sm' }, it.local_release_format || '—'),
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
  ]);
}

function PosterCard(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const poster = it.local_poster_url || '';
  const onclick = () => m.route.set('/recordings/' + it.id);
  const placeholder = m('div', {
    class: 'aspect-[2/3] w-full bg-base-300 flex items-center justify-center text-xs opacity-60 px-2 text-center',
  }, m('span', { class: 'badge ' + meta.badge }, meta.label));
  const image = m('img', {
    src: poster,
    alt: it.show || '',
    loading: 'lazy',
    class: 'aspect-[2/3] w-full object-cover',
  });
  return m('div', {
    // See Row's `key` comment — same reasoning. The grid view's
    // PosterCards each fire their own poster network request, so a
    // keyed diff is doubly important here: without it, every
    // back-navigation refresh re-fetches all 50 thumbnails.
    key: 'rec-' + it.id,
    class: 'card bg-base-200 shadow-sm hover:shadow-md hover:ring-1 hover:ring-primary cursor-pointer transition-shadow',
    onclick,
    role: 'button',
    tabindex: 0,
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); onclick(); }
    },
  }, [
    m('div', { class: 'indicator w-full' }, [
      m('span', {
        class: 'indicator-item badge badge-sm ' + meta.badge,
      }, meta.label),
      m('div', { class: 'overflow-hidden rounded-t-box w-full' },
        poster ? image : placeholder,
      ),
    ]),
    m('div', { class: 'card-body p-2 gap-0.5' }, [
      m('div', { class: 'text-sm font-medium truncate', title: it.show || '' },
        it.show || '—'),
      m('div', { class: 'text-xs opacity-60 font-mono truncate' },
        (it.tour ? it.tour + ' · ' : '') +
        smartDateWithVariant(
          it.date_full, it.date_month_known, it.date_day_known, it.date_variant)),
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
      m('thead', m('tr',
        SORT_COLUMNS.map((col) => HeaderCell(col, sortKey, sortDir)),
      )),
      m('tbody', items.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
          }, 'No recordings match this filter.'))
        : items.map(Row)),
    ]));
}

function renderGrid(items) {
  if (items.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No recordings match this filter.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, items.map(PosterCard));
}

const Recordings = {
  oninit() {
    readURLParams();
    loadRecordings();
  },

  // oncreate restores the saved scroll position after the grid/list
  // body has laid out, so back-navigation from a recording detail
  // keeps the user where they left off. main.js disables the
  // browser's auto restoration; this hook owns it instead.
  oncreate() {
    const y = state.recordings.scrollY;
    if (typeof y === 'number' && y > 0 && typeof window !== 'undefined') {
      window.scrollTo(0, y);
    }
  },

  // onbeforeremove captures scrollY so the next mount of this page
  // can restore it. Returning a resolved Promise is unnecessary —
  // we don't need to delay the unmount.
  onbeforeremove() {
    if (typeof window !== 'undefined') {
      state.recordings.scrollY = window.scrollY || 0;
    }
  },

  // onupdate fires on every redraw; readURLParams + a state-buttonshot
  // diff lets us refetch only when the inputs that affect the result
  // set actually changed (view toggles within the same dataset don't
  // need a network round-trip). The URL is the source of truth for
  // status/sort/dir; local state mirrors it.
  onupdate() {
    const r = state.recordings;
    const before = {
      status: r.status,
      sortKey: r.sortKey,
      sortDir: r.sortDir,
    };
    readURLParams();
    if (before.status !== r.status ||
        before.sortKey !== r.sortKey ||
        before.sortDir !== r.sortDir) {
      loadRecordings();
    }
  },

  view() {
    const r = state.recordings;

    if (r.loading && r.items.length === 0) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading recordings…');
    }
    if (r.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' },
        m('span', 'Failed to load recordings: ' + (r.error.message || r.error)));
    }

    const total = r.total;
    const headerSub = total === 0
      ? 'No recordings loaded'
      : String(total) + ' recording' + (total === 1 ? '' : 's') +
        (r.status ? ' · status: ' + r.status : '');

    const body = r.view === 'grid'
      ? renderGrid(r.items)
      : renderList(r.items, r.sortKey, r.sortDir);

    return m('div', { class: 'space-y-6' }, [
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Recordings'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, headerSub),
        ]),
        m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
          ViewToggle(r.view),
          m('button', { type: 'button', class: 'btn btn-ghost btn-sm', disabled: true }, 'Filters'),
          m('button', { type: 'button', class: 'btn btn-sm', disabled: true }, 'Sync now'),
          m('button', { type: 'button', class: 'btn btn-primary btn-sm', disabled: true }, 'Manual import'),
        ]),
      ]),

      m('div', { role: 'tablist', class: 'tabs tabs-box' },
        STATUS_FILTERS.map((f) => Tab(f, r.status))),

      body,
    ]);
  },
};

export default Recordings;
