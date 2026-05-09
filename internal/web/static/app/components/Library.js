// Library.js — Mithril port of the legacy /static/library.js.
//
// Keeps the visual structure: page header w/ sub-text + action buttons,
// 4 metric tiles, status filter tabs, sortable table. Status taxonomy
// is the LOWERCASE storage.Status set ('synced', 'format_mismatch',
// 'missing', 'wanted', 'orphan'). URL params persist the filter / sort
// state via m.route.set so deep links keep working.
//
// Two orthogonal view dimensions live alongside the filters:
//   mode  : 'recordings' | 'shows'  → what the rows ARE
//   view  : 'list'       | 'grid'   → how the rows are RENDERED
// Both persist as URL params so a deep link survives reload. The grid
// view leans on cached posters (item.local_poster_url for recordings,
// item.local_poster_url on shows from /api/v1/shows) and falls back to
// a status-tinted placeholder card when no poster is on disk yet.
//
// Default sort: by recording (show name + date asc) for recordings;
// by name asc for shows. Each mode owns its own SORT_COLUMNS table so
// the URL ?sort= token is interpreted against the right column set.

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

// SORT_COLUMNS lists every sortable column for the recordings mode.
// `key` is the URL token; `compare` is a stable comparator.
const SORT_COLUMNS = [
  { key: 'status',       label: 'Status',
    compare: (a, b) => cmpStr(a.status, b.status) },
  { key: 'recording',    label: 'Recording',
    compare: (a, b) => {
      // Primary: show name. Within a show, sort chronologically so
      // multiple Halcyon Crossing recordings group naturally and read in
      // performance order. Tour + id tiebreak when dates collide.
      let c = cmpStr(a.show, b.show); if (c) return c;
      c = cmpStr(a.date_full, b.date_full); if (c) return c;
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

// SORT_COLUMNS_SHOWS is the by-show variant. Different shape, smaller
// set of meaningful axes. Year columns coerce to 0 when null so a
// show without dated recordings pins to the bottom of an asc sort.
const SORT_COLUMNS_SHOWS = [
  { key: 'name',            label: 'Show',
    compare: (a, b) => cmpStr(a.name, b.name) },
  { key: 'recording_count', label: 'Recordings',
    compare: (a, b) => cmpNum(a.recording_count, b.recording_count) },
  { key: 'first_year',      label: 'First',
    compare: (a, b) => cmpNum(a.first_year || 0, b.first_year || 0) },
  { key: 'last_year',       label: 'Last',
    compare: (a, b) => cmpNum(a.last_year || 0, b.last_year || 0) },
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
function sortItems(items, sort, columns) {
  const cols = columns || SORT_COLUMNS;
  const col = cols.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => {
    if (col.key === 'local_format') return col.compare(a, b);
    return sign * col.compare(a, b);
  });
  return out;
}

// readURLParams pulls the active status / sort / view / mode state out
// of the current Mithril route.
function readURLParams() {
  const params = m.route.param() || {};
  const lib = state.library;

  // Mode comes first because the sort vocabulary depends on it.
  const rawMode = (params.mode || '').toLowerCase();
  lib.mode = rawMode === 'shows' ? 'shows' : 'recordings';

  const rawView = (params.view || '').toLowerCase();
  lib.view = rawView === 'grid' ? 'grid' : 'list';

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
  m.route.set('/', out, { replace: true });
}

// setStatus updates the active filter and re-syncs the URL.
function setStatus(key) {
  state.library.status = key;
  pushURLParams();
}

// setView toggles between list and grid. Sort + filter survive the
// switch since both views render the same dataset.
function setView(key) {
  if (key !== 'list' && key !== 'grid') return;
  state.library.view = key;
  pushURLParams();
}

// setMode toggles between recordings and shows. The sort axis flips
// to that mode's default since the column set is mode-specific. shows
// data is fetched lazily and cached across flips.
function setMode(key) {
  if (key !== 'recordings' && key !== 'shows') return;
  const lib = state.library;
  if (lib.mode === key) return;
  lib.mode = key;
  const def = defaultSortForMode(key);
  lib.sortKey = def.key;
  lib.sortDir = def.dir;
  pushURLParams();
  if (key === 'shows') ensureShowsLoaded();
}

// ensureShowsLoaded fetches /api/v1/shows the first time the user
// flips to the shows mode. Result is cached on state.library.shows so
// flipping back-and-forth doesn't refetch.
function ensureShowsLoaded() {
  const lib = state.library;
  if (lib.shows.length > 0 || lib.showsLoading) return;
  lib.showsLoading = true;
  lib.showsError = null;
  api.get('/shows').then((body) => {
    lib.shows = (body && body.items) || [];
    lib.showsLoading = false;
  }).catch((err) => {
    lib.showsError = err;
    lib.showsLoading = false;
  });
}

// setSort toggles direction when the user clicks the active column,
// otherwise drops to that column's default direction (defaultDirForKey).
function setSort(key) {
  const lib = state.library;
  if (lib.sortKey === key) {
    lib.sortDir = lib.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    lib.sortKey = key;
    lib.sortDir = defaultDirForKey(key);
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

// PosterCard renders one recording-grid cell. Falls back to a
// placeholder block (status-tinted) when no cached poster is on disk.
// The status badge is corner-pinned via DaisyUI `indicator`.
function PosterCard(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const poster = it.local_poster_url || '';
  const onclick = () => m.route.set('/recordings/' + it.id);
  // 2:3 aspect ratio matches a real movie poster; the empty
  // placeholder uses the same ratio so the grid stays even.
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
    class: 'card bg-base-200 shadow-sm hover:shadow-md hover:ring-1 hover:ring-primary cursor-pointer transition-shadow overflow-hidden',
    onclick,
    role: 'button',
    tabindex: 0,
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); onclick(); }
    },
  }, [
    m('div', { class: 'indicator w-full' }, [
      m('span', {
        class: 'indicator-item indicator-top indicator-end badge badge-sm ' + meta.badge,
      }, meta.label),
      poster ? image : placeholder,
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
    class: 'card bg-base-200 shadow-sm hover:shadow-md hover:ring-1 hover:ring-primary cursor-pointer transition-shadow overflow-hidden',
    onclick,
    role: 'button',
    tabindex: 0,
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); onclick(); }
    },
  }, [
    poster ? image : placeholder,
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
function renderRecordingsList(sorted, sortKey, sortDir) {
  return m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
    m('table', { class: 'table table-zebra' }, [
      m('thead', m('tr',
        SORT_COLUMNS.map((col) => HeaderCell(col, sortKey, sortDir)),
      )),
      m('tbody', sorted.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
          }, 'No recordings match this filter.'))
        : sorted.map(Row)),
    ]));
}

// renderRecordingsGrid renders the responsive poster grid.
function renderRecordingsGrid(sorted) {
  if (sorted.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No recordings match this filter.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, sorted.map(PosterCard));
}

// renderShowsList renders the by-show table.
function renderShowsList(sorted, sortKey, sortDir) {
  return m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
    m('table', { class: 'table table-zebra' }, [
      m('thead', m('tr', [
        ...SORT_COLUMNS_SHOWS.slice(0, 2).map((col) => HeaderCell(col, sortKey, sortDir)),
        m('th', 'States'),
        // first/last folded into one "Years" sortable header — it
        // just sorts on whichever the user asked for via sort param;
        // we surface the two columns separately to keep table sort
        // legible.
        ...SORT_COLUMNS_SHOWS.slice(2).map((col) => HeaderCell(col, sortKey, sortDir)),
      ])),
      m('tbody', sorted.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS_SHOWS.length + 1,
            class: 'text-center opacity-60 py-8',
          }, 'No shows in the catalog yet.'))
        : sorted.map((s) => m('tr', {
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
function renderShowsGrid(sorted) {
  if (sorted.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No shows in the catalog yet.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, sorted.map(ShowCard));
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
    if (state.library.mode === 'shows') ensureShowsLoaded();
  },

  // onupdate fires on every route change while we stay mounted. The
  // SPA uses Library for `/` only, so any update implies the
  // querystring (status / sort / dir / view / mode) may have changed.
  // Re-read, kick off a shows fetch if mode just flipped.
  onupdate() {
    readURLParams();
    if (state.library.mode === 'shows') ensureShowsLoaded();
  },

  view() {
    const lib = state.library;

    if (lib.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading library…');
    }
    if (lib.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load recordings: ' + (lib.error.message || lib.error)),
      ]);
    }

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

    const filtered = lib.status
      ? items.filter((it) => it.status === lib.status)
      : items.slice();
    const sortedRecordings = sortItems(filtered,
      { key: lib.sortKey, dir: lib.sortDir }, SORT_COLUMNS);
    const sortedShows = sortItems(lib.shows,
      { key: lib.sortKey, dir: lib.sortDir }, SORT_COLUMNS_SHOWS);

    let body;
    if (lib.mode === 'shows') {
      if (lib.showsLoading && lib.shows.length === 0) {
        body = m('div', { class: 'p-8 opacity-60' }, 'Loading shows…');
      } else if (lib.showsError) {
        body = m('div', { role: 'alert', class: 'alert alert-error' },
          m('span', 'Failed to load shows: ' +
            (lib.showsError.message || lib.showsError)));
      } else if (lib.view === 'grid') {
        body = renderShowsGrid(sortedShows);
      } else {
        body = renderShowsList(sortedShows, lib.sortKey, lib.sortDir);
      }
    } else if (lib.view === 'grid') {
      body = renderRecordingsGrid(sortedRecordings);
    } else {
      body = renderRecordingsList(sortedRecordings, lib.sortKey, lib.sortDir);
    }

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Library'),
          m('p', { class: 'text-sm opacity-70 mt-1' },
            total + ' cataloged · ' + synced + ' synced · ' +
            wanted + ' wanted · ' + files + ' files on disk'),
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

      // Stat tiles. `stats` parent wraps `stat` children per DaisyUI.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Synced',     synced,         total + ' cataloged'),
        MetricTile('Wanted',     wanted,         'on the shopping list'),
        MetricTile('Mismatches', mismatchTotal,  'needs reconcile'),
        MetricTile('Files on disk', files,       'across all versions'),
      ]),

      // Status filter tabs only apply to recordings mode — by-show
      // aggregates are intrinsically multi-status.
      lib.mode === 'recordings'
        ? m('div', { role: 'tablist', class: 'tabs tabs-box' },
            STATUS_FILTERS.map((f) => Tab(f, counts, lib.status)))
        : null,

      body,
    ]);
  },
};

export default Library;
