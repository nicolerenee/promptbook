// Library.js — Mithril port of the legacy /static/library.js.
//
// Keeps the visual structure: page header w/ sub-text + action buttons,
// 4 metric tiles, status filter tabs, sortable + paginated table.
// Status taxonomy is the LOWERCASE storage.Status set ('synced',
// 'format_mismatch', 'missing', 'wanted', 'orphan'). URL params
// persist the filter / sort / page state via m.route.set so deep
// links keep working.
//
// Two orthogonal view dimensions live alongside the filters:
//   mode  : 'recordings' | 'shows'  → what the rows ARE
//   view  : 'list'       | 'grid'   → how the rows are RENDERED
// Both persist as URL params so a deep link survives reload. The grid
// view leans on cached posters (item.local_poster_url for recordings,
// item.local_poster_url on shows from /api/v1/shows) and falls back to
// a status-tinted placeholder card when no poster is on disk yet.
//
// Pagination + sort moved to the server. Each mode owns its own
// (offset, total) state slice (recordings + shows have different
// row counts) and its own sort column whitelist. The shared
// Pagination component drives Prev / Next; flipping a filter, sort,
// or mode resets offset to 0.
//
// Stat tiles previously summed counts off the visible page. With
// pagination that's wrong — they'd read "1 synced" because only one
// row on the current page is synced. The handler doesn't yet ship a
// status-totals payload, so the tiles read off the loaded shows
// dataset (which has state_counts per show) when the mode is
// recordings, falling back to "—" when no shows are loaded yet. A
// cleaner fix is server-side aggregate totals, but that's a separate
// PR.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate } from '../utils/format.js';
import Pagination from './Pagination.js';

// LS_VIEW + LS_MODE are the localStorage keys the Library page uses to
// persist the user's preferred layout across visits. The URL still
// wins when it carries an explicit ?view=/?mode= (so deep links work);
// localStorage is the fallback when the URL is bare. Sort + status +
// page are intentionally NOT persisted — they're query-driven and
// would surprise the user if they survived navigation.
const LS_VIEW = 'pb.library.view';
const LS_MODE = 'pb.library.mode';

// readStoredView / readStoredMode return the persisted preference, or
// '' when nothing is stored or localStorage is unavailable (private
// mode, server-side render, etc.). The empty-string sentinel keeps the
// caller branching simple — falsy means "no preference".
function readStoredView() {
  try {
    const v = window.localStorage.getItem(LS_VIEW);
    return v === 'grid' || v === 'list' ? v : '';
  } catch (_) {
    return '';
  }
}
function readStoredMode() {
  try {
    const v = window.localStorage.getItem(LS_MODE);
    return v === 'shows' || v === 'recordings' ? v : '';
  } catch (_) {
    return '';
  }
}
function writeStoredView(v) {
  try { window.localStorage.setItem(LS_VIEW, v); } catch (_) { /* no-op */ }
}
function writeStoredMode(v) {
  try { window.localStorage.setItem(LS_MODE, v); } catch (_) { /* no-op */ }
}

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

// SORT_COLUMNS lists every sortable column for the recordings mode.
// `key` is the URL token and the server-side whitelist key.
const SORT_COLUMNS = [
  { key: 'status',       label: 'Status' },
  { key: 'recording',    label: 'Recording' },
  { key: 'date',         label: 'Date' },
  { key: 'master',       label: 'Master' },
  { key: 'local_format', label: 'Local format' },
];

// SORT_COLUMNS_SHOWS is the by-show variant. Different shape, smaller
// set of meaningful axes.
const SORT_COLUMNS_SHOWS = [
  { key: 'name',            label: 'Show' },
  { key: 'recording_count', label: 'Recordings' },
  { key: 'first_year',      label: 'First' },
  { key: 'last_year',       label: 'Last' },
];

// Default sort: by recording (show name + date asc within show). Reads
// like a phone-book grouped by show, with each show's recordings in
// chronological order.
const DEFAULT_SORT = { key: 'recording', dir: 'asc' };
const DEFAULT_SORT_SHOWS = { key: 'name', dir: 'asc' };

function sortColumnsForMode(mode) {
  return mode === 'shows' ? SORT_COLUMNS_SHOWS : SORT_COLUMNS;
}

function defaultSortForMode(mode) {
  return mode === 'shows' ? DEFAULT_SORT_SHOWS : DEFAULT_SORT;
}

// defaultDirForKey returns the direction a *fresh* click on a column
// header should drop to. Mirrors legacy library.js: 'date' / 'last_year'
// default to desc (most-recent first reads better), counts default to
// desc (biggest first), everything else asc.
function defaultDirForKey(key) {
  if (key === 'date' || key === 'last_year' || key === 'recording_count') return 'desc';
  return 'asc';
}

// readURLParams pulls the active status / sort / view / mode / page
// state out of the current Mithril route. View + mode fall back to
// the user's persisted preference (localStorage) when the URL has no
// explicit value, so navigating to bare `/` after picking grid+shows
// once restores that layout instead of resetting to list+recordings.
function readURLParams() {
  const params = m.route.param() || {};
  const lib = state.library;

  // Mode comes first because the sort vocabulary depends on it.
  // URL param wins; otherwise consult localStorage; otherwise default.
  const rawMode = (params.mode || '').toLowerCase();
  if (rawMode === 'shows' || rawMode === 'recordings') {
    lib.mode = rawMode;
  } else {
    lib.mode = readStoredMode() || 'recordings';
  }

  const rawView = (params.view || '').toLowerCase();
  if (rawView === 'grid' || rawView === 'list') {
    lib.view = rawView;
  } else {
    lib.view = readStoredView() || 'list';
  }

  const rawStatus = (params.status || '').toLowerCase();
  const canon = STATUS_FILTERS.find((f) => f.key.toLowerCase() === rawStatus);
  lib.status = canon ? canon.key : '';

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const cols = sortColumnsForMode(lib.mode);
  const col = cols.find((c) => c.key === sortKey);
  if (!col) {
    const def = defaultSortForMode(lib.mode);
    lib.sortKey = def.key;
    lib.sortDir = def.dir;
  } else {
    lib.sortKey = col.key;
    lib.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }

  // Page is 1-indexed in the URL for human readability; convert to
  // 0-indexed offset for the API. Each mode tracks its own offset.
  const page = parseInt(params.page, 10);
  const offset = (page > 1) ? (page - 1) * lib.limit : 0;
  if (lib.mode === 'shows') {
    lib.showsOffset = offset;
  } else {
    lib.offset = offset;
  }
}

// pushURLParams syncs state.library back to the browser URL so a
// reload or share keeps the same view. Defaults are dropped.
function pushURLParams() {
  const lib = state.library;
  const out = {};
  if (lib.status) out.status = lib.status;
  if (lib.view !== 'list') out.view = lib.view;
  if (lib.mode !== 'recordings') out.mode = lib.mode;
  out.sort = lib.sortKey;
  out.dir = lib.sortDir;
  const offset = lib.mode === 'shows' ? lib.showsOffset : lib.offset;
  const page = Math.floor(offset / lib.limit) + 1;
  if (page > 1) out.page = String(page);
  m.route.set('/', out, { replace: true });
}

// loadRecordings fetches the active page of /api/v1/recordings using
// the current sort + status filter.
function loadRecordings() {
  const lib = state.library;
  lib.loading = true;
  lib.error = null;
  const params = new URLSearchParams();
  params.set('limit', String(lib.limit));
  params.set('offset', String(lib.offset));
  params.set('sort', lib.sortKey);
  params.set('dir', lib.sortDir);
  if (lib.status) params.set('status', lib.status);
  return api.get('/recordings?' + params.toString()).then((body) => {
    lib.items = (body && body.items) || [];
    lib.total = (body && body.total) || 0;
    lib.loading = false;
  }).catch((err) => {
    lib.error = err;
    lib.loading = false;
  });
}

// loadShows fetches the active page of /api/v1/shows. Sort + offset
// for the shows mode are tracked separately on state.library.
function loadShows() {
  const lib = state.library;
  lib.showsLoading = true;
  lib.showsError = null;
  const params = new URLSearchParams();
  params.set('limit', String(lib.limit));
  params.set('offset', String(lib.showsOffset));
  // shows mode uses its own sort vocabulary; only forward the key
  // when the active sort is a shows column (otherwise the server
  // falls back to default = name asc, which is what we want).
  if (SORT_COLUMNS_SHOWS.find((c) => c.key === lib.sortKey)) {
    params.set('sort', lib.sortKey);
    params.set('dir', lib.sortDir);
  }
  return api.get('/shows?' + params.toString()).then((body) => {
    lib.shows = (body && body.items) || [];
    lib.showsTotal = (body && body.total) || 0;
    lib.showsLoading = false;
  }).catch((err) => {
    lib.showsError = err;
    lib.showsLoading = false;
  });
}

// loadActive refetches whichever mode is active.
function loadActive() {
  const lib = state.library;
  if (lib.mode === 'shows') return loadShows();
  return loadRecordings();
}

// setStatus updates the active filter, resets offset to 0 (filter
// changes shrink the dataset), and refetches.
function setStatus(key) {
  const lib = state.library;
  lib.status = key;
  lib.offset = 0;
  pushURLParams();
  loadRecordings();
}

// setView toggles between list and grid. Sort + filter survive the
// switch since both views render the same dataset. Persisted to
// localStorage so the choice survives navigation away and back.
function setView(key) {
  if (key !== 'list' && key !== 'grid') return;
  state.library.view = key;
  writeStoredView(key);
  pushURLParams();
}

// setMode toggles between recordings and shows. The sort axis flips
// to that mode's default since the column set is mode-specific.
// Persisted to localStorage alongside view.
function setMode(key) {
  if (key !== 'recordings' && key !== 'shows') return;
  const lib = state.library;
  if (lib.mode === key) return;
  lib.mode = key;
  writeStoredMode(key);
  const def = defaultSortForMode(key);
  lib.sortKey = def.key;
  lib.sortDir = def.dir;
  pushURLParams();
  loadActive();
}

// setSort toggles direction when the user clicks the active column,
// otherwise drops to that column's default direction (defaultDirForKey).
// Resets offset to 0 since the row at offset N changes meaning when
// the sort axis flips.
function setSort(key) {
  const lib = state.library;
  if (lib.sortKey === key) {
    lib.sortDir = lib.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    lib.sortKey = key;
    lib.sortDir = defaultDirForKey(key);
  }
  if (lib.mode === 'shows') {
    lib.showsOffset = 0;
  } else {
    lib.offset = 0;
  }
  pushURLParams();
  loadActive();
}

// setOffsetRecordings / setOffsetShows are the Pagination component
// callbacks. Each mode has its own offset because flipping mode swaps
// the dataset entirely.
function setOffsetRecordings(newOffset) {
  state.library.offset = newOffset;
  pushURLParams();
  loadRecordings();
}
function setOffsetShows(newOffset) {
  state.library.showsOffset = newOffset;
  pushURLParams();
  loadShows();
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
// the "pill row" we want; tab-active marks the chosen one. Counts
// come from a separate aggregate call (or are omitted) — with
// pagination we no longer have a "count by status on the current
// page" that's meaningful; the active status's count is shown via
// the page indicator instead.
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

// Row renders one recording. Click navigates to the detail page via
// the SPA router.
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

// stateBadgeCluster renders a tight row of small status pills, one
// per non-zero state bucket. Used in the by-show list (state column)
// and the grid card so the reader sees at-a-glance what's covered for
// a show. Order follows STATUS_FILTERS so the visual rhythm is stable
// across rows.
function stateBadgeCluster(stateCounts) {
  if (!stateCounts) return [];
  const out = [];
  STATUS_FILTERS.forEach((f) => {
    if (!f.key) return;
    const n = stateCounts[f.key] || 0;
    if (n === 0) return;
    const meta = STATUS_META[f.key] || STATUS_META.orphan;
    out.push(m('span', {
      class: 'badge badge-xs ' + meta.badge,
      title: meta.label + ': ' + n,
    }, n));
  });
  return out;
}

// yearSpan renders a "1995–2024" string when first/last differ, the
// single year when they match, or '—' when neither was provided.
function yearSpan(first, last) {
  if (first == null && last == null) return '—';
  if (first == null) return String(last);
  if (last == null) return String(first);
  if (first === last) return String(first);
  return first + '–' + last;
}

// PosterCard renders one recording-grid cell. local_poster_url is
// ALWAYS the canonical /images/... path when image caching is on —
// the server falls through to the SVG placeholder generator on cache
// miss, so the browser always gets a valid image.
//
// The status badge uses DaisyUI's `indicator` pattern: the badge is
// translated outside the indicator's content box, so the CARD itself
// must NOT have overflow-hidden — that was the previous bug,
// clipping the badge. Rounded corners on the image come from the
// inner image-clip div (`overflow-hidden rounded-t-box`), which only
// clips the image, not the indicator's outflow.
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
        smartDate(it.date_full, it.date_month_known, it.date_day_known)),
    ]),
  ]);
}

// ShowCard renders one show-grid cell. Same poster-or-placeholder
// pattern as PosterCard, but uses the show poster + recording-count
// label and shows the state cluster across the bottom.
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

// IconList / IconGrid are small inline SVGs rather than an icon-font
// dependency. 18px squares match the btn-sm icon footprint DaisyUI
// uses for its own button-with-icon snippets.
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

// ModeTabs renders the Recordings / Shows toggle (DaisyUI tabs-box).
// Lives inline in the page header.
function ModeTabs(mode) {
  return m('div', { role: 'tablist', class: 'tabs tabs-box tabs-sm' }, [
    m('a', {
      role: 'tab',
      class: 'tab' + (mode === 'recordings' ? ' tab-active' : ''),
      onclick: (ev) => { ev.preventDefault(); setMode('recordings'); },
      href: '#',
    }, 'Recordings'),
    m('a', {
      role: 'tab',
      class: 'tab' + (mode === 'shows' ? ' tab-active' : ''),
      onclick: (ev) => { ev.preventDefault(); setMode('shows'); },
      href: '#',
    }, 'Shows'),
  ]);
}

// ViewToggle renders the list/grid icon-button pair as a `join` (the
// DaisyUI segmented-button shape). Active state mirrors btn-active.
function ViewToggle(view) {
  return m('div', {
    class: 'join',
    role: 'group',
    'aria-label': 'View mode',
  }, [
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

// renderRecordingsList renders the existing sortable table.
function renderRecordingsList(items, sortKey, sortDir) {
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

// renderRecordingsGrid renders the responsive poster grid.
function renderRecordingsGrid(items) {
  if (items.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No recordings match this filter.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, items.map(PosterCard));
}

// renderShowsList renders the by-show table.
function renderShowsList(items, sortKey, sortDir) {
  return m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
    m('table', { class: 'table table-zebra' }, [
      m('thead', m('tr', [
        ...SORT_COLUMNS_SHOWS.slice(0, 2).map((col) => HeaderCell(col, sortKey, sortDir)),
        m('th', 'States'),
        ...SORT_COLUMNS_SHOWS.slice(2).map((col) => HeaderCell(col, sortKey, sortDir)),
      ])),
      m('tbody', items.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS_SHOWS.length + 1,
            class: 'text-center opacity-60 py-8',
          }, 'No shows in the catalog yet.'))
        : items.map((s) => m('tr', {
            class: 'hover:bg-base-200 cursor-pointer',
            onclick: () => m.route.set('/shows/' + s.id),
          }, [
            m('td', m('div', { class: 'font-medium' }, s.name || '—')),
            m('td', { class: 'font-mono text-sm' }, String(s.recording_count || 0)),
            m('td', m('div', { class: 'flex flex-wrap gap-1' },
              stateBadgeCluster(s.state_counts))),
            m('td', { class: 'font-mono text-sm' },
              s.first_year == null ? '—' : String(s.first_year)),
            m('td', { class: 'font-mono text-sm' },
              s.last_year == null ? '—' : String(s.last_year)),
          ]))),
    ]));
}

// renderShowsGrid renders the responsive show-poster grid.
function renderShowsGrid(items) {
  if (items.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No shows in the catalog yet.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, items.map(ShowCard));
}

const Library = {
  oninit() {
    readURLParams();
    loadActive();
  },

  // onupdate fires on every route change while we stay mounted. The
  // SPA uses Library for `/` only, so any update implies the
  // querystring (status / sort / dir / view / mode / page) may have
  // changed.
  onupdate() {
    const lib = state.library;
    const before = {
      mode: lib.mode,
      status: lib.status,
      sortKey: lib.sortKey,
      sortDir: lib.sortDir,
      offset: lib.mode === 'shows' ? lib.showsOffset : lib.offset,
    };
    readURLParams();
    const afterOffset = lib.mode === 'shows' ? lib.showsOffset : lib.offset;
    // Refetch only when the inputs that affect the result set
    // changed — view/mode toggles within the same dataset don't need
    // a network round-trip.
    if (before.mode !== lib.mode ||
        before.status !== lib.status ||
        before.sortKey !== lib.sortKey ||
        before.sortDir !== lib.sortDir ||
        before.offset !== afterOffset) {
      loadActive();
    }
  },

  view() {
    const lib = state.library;
    const isShowsMode = lib.mode === 'shows';

    // Render skeleton on first load (no items yet); subsequent
    // refetches keep the previous page visible so the user doesn't
    // see a flash of "Loading…" between page clicks.
    if (isShowsMode) {
      if (lib.showsLoading && lib.shows.length === 0) {
        return m('div', { class: 'p-8 opacity-60' }, 'Loading shows…');
      }
      if (lib.showsError) {
        return m('div', { role: 'alert', class: 'alert alert-error' },
          m('span', 'Failed to load shows: ' +
            (lib.showsError.message || lib.showsError)));
      }
    } else {
      if (lib.loading && lib.items.length === 0) {
        return m('div', { class: 'p-8 opacity-60' }, 'Loading library…');
      }
      if (lib.error) {
        return m('div', { role: 'alert', class: 'alert alert-error' },
          m('span', 'Failed to load recordings: ' + (lib.error.message || lib.error)));
      }
    }

    const total = isShowsMode ? lib.showsTotal : lib.total;
    const offset = isShowsMode ? lib.showsOffset : lib.offset;
    const start = total === 0 ? 0 : offset + 1;
    const end = Math.min(offset + lib.limit, total);

    let body;
    if (isShowsMode) {
      body = lib.view === 'grid'
        ? renderShowsGrid(lib.shows)
        : renderShowsList(lib.shows, lib.sortKey, lib.sortDir);
    } else if (lib.view === 'grid') {
      body = renderRecordingsGrid(lib.items);
    } else {
      body = renderRecordingsList(lib.items, lib.sortKey, lib.sortDir);
    }

    // Stat tiles — totals are off the active list (status filter ON
    // when set). With server-side pagination we no longer have a
    // status-by-status breakdown of the full library; the tiles read
    // off the current dataset's total instead so the user gets a
    // meaningful "X of Y" with the active filter applied.
    const headerLabel = isShowsMode ? 'shows' : 'recordings';
    const headerSub = total === 0
      ? 'No ' + headerLabel + ' loaded'
      : 'Showing ' + start + '–' + end + ' of ' + total + ' ' + headerLabel +
        (lib.status ? ' · status: ' + lib.status : '');

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Library'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, headerSub),
        ]),
        m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
          ModeTabs(lib.mode),
          ViewToggle(lib.view),
          // Action buttons — cosmetic stubs until later waves wire them.
          m('button', { type: 'button', class: 'btn btn-ghost btn-sm', disabled: true }, 'Filters'),
          m('button', { type: 'button', class: 'btn btn-sm', disabled: true }, 'Sync now'),
          m('button', { type: 'button', class: 'btn btn-primary btn-sm', disabled: true }, 'Manual import'),
        ]),
      ]),

      // Stat tiles — kept around for visual rhythm. With pagination
      // the global counts are not available without an extra
      // aggregate call; show the active total + active mode/filter
      // summary so the tiles still anchor the page.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile(isShowsMode ? 'Shows' : 'Recordings', total,
          lib.status ? 'matching ' + lib.status : 'in catalog'),
        MetricTile('Page size', lib.limit, '50/page default'),
        MetricTile('Mode', isShowsMode ? 'Shows' : 'Recordings',
          isShowsMode ? 'aggregated' : 'per-recording'),
        MetricTile('Page', Math.floor(offset / lib.limit) + 1,
          'of ' + Math.max(1, Math.ceil(total / lib.limit))),
      ]),

      // Status filter tabs only apply to recordings mode — by-show
      // aggregates are intrinsically multi-status.
      !isShowsMode
        ? m('div', { role: 'tablist', class: 'tabs tabs-box' },
            STATUS_FILTERS.map((f) => Tab(f, lib.status)))
        : null,

      body,

      // Pagination strip — each mode owns its own offset/total.
      m(Pagination, {
        offset,
        limit: lib.limit,
        total,
        setOffset: isShowsMode ? setOffsetShows : setOffsetRecordings,
      }),
    ]);
  },
};

export default Library;
