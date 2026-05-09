// People.js — Mithril port of the legacy /static/people.js.
//
// Renders the /people index: page header w/ count + "showing N of M",
// search input that filters CURRENT-PAGE rows client-side (so the
// keypresses give instant feedback even though they only narrow the
// 50 visible rows), and a sortable table of performers (avatar, name,
// recording count, per-state badge cluster). Clicking a row routes to
// /people/:id.
//
// Pagination + sort live on the server now — flipping a column header
// rebuilds the URL with sort/dir + page=1, the SPA refetches, and the
// shared Pagination component drives the Prev / Next buttons. Search
// stays client-side on purpose: filtering the visible page gives an
// instant "did I spell the name right?" loop, and the user can hit
// Next to see the rest. A future wave can promote search to a server-
// side query if 50-row pages stop being enough context.
//
// Avatar fallback: /api/v1/people doesn't expose headshot_url on the
// list (only the detail endpoint does). We render DaisyUI's
// avatar-placeholder with the performer's initials in every row to
// keep the column shape consistent — the detail page is where we
// upgrade to a real headshot.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import Pagination from './Pagination.js';

// STATUS_META mirrors Library.js so the badge cluster colour-codes the
// per-state breakdown the same way the library page does.
const STATUS_META = {
  synced:          { label: 'Synced',          short: 'Sync',     badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', short: 'Mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         short: 'Missing',  badge: 'badge-error' },
  wanted:          { label: 'Wanted',          short: 'Wanted',   badge: 'badge-info' },
  orphan:          { label: 'Orphan',          short: 'Orphan',   badge: 'badge-neutral' },
};

// STATE_ORDER fixes the badge cluster order — synced first so the most
// common state reads left-to-right, mismatch/missing/wanted/orphan
// after.
const STATE_ORDER = ['synced', 'format_mismatch', 'missing', 'wanted', 'orphan'];

// SORT_COLUMNS lists the sortable headers in render order. `key` is
// both URL token + the column id; the server resolves it against a
// whitelist (peopleSortFragments) and falls back to default on miss.
const SORT_COLUMNS = [
  { key: 'name',  label: 'Performer' },
  { key: 'count', label: 'Recordings' },
];

const DEFAULT_SORT = { key: 'name', dir: 'asc' };

// monogram returns up to 2 initial characters from the performer's
// name. "Eva Noblezada" → "EN", "Cher" → "C", empty → "?". Mirrors the
// legacy people.js implementation so the typographic fallback reads
// the same.
function monogram(name) {
  if (!name) return '?';
  const parts = String(name).trim().split(/\s+/);
  let letters = '';
  for (let i = 0; i < parts.length && letters.length < 2; i++) {
    if (parts[i].length > 0) letters += parts[i][0];
  }
  return letters.toUpperCase() || '?';
}

// readURLParams pulls the active search/sort/page state out of the
// current Mithril route. Empty params land on defaults.
function readURLParams() {
  const params = m.route.param() || {};
  const p = state.people;

  p.q = params.q || '';

  const sortKey = params.sort || '';
  const dir = (params.dir || '').toLowerCase();
  const col = SORT_COLUMNS.find((c) => c.key === sortKey);
  if (!col) {
    p.sortKey = DEFAULT_SORT.key;
    p.sortDir = DEFAULT_SORT.dir;
  } else {
    p.sortKey = col.key;
    p.sortDir = (dir === 'asc' || dir === 'desc') ? dir : 'asc';
  }

  const page = parseInt(params.page, 10);
  p.offset = (page > 1) ? (page - 1) * p.limit : 0;
}

// pushURLParams syncs state.people back to the browser URL so a reload
// or share keeps the same filtered/sorted view. Empty values are
// dropped; page=1 is dropped since it's the default.
function pushURLParams() {
  const p = state.people;
  const out = {};
  if (p.q) out.q = p.q;
  if (p.sortKey !== DEFAULT_SORT.key || p.sortDir !== DEFAULT_SORT.dir) {
    out.sort = p.sortKey;
    out.dir = p.sortDir;
  }
  const page = Math.floor(p.offset / p.limit) + 1;
  if (page > 1) out.page = String(page);
  m.route.set('/people', out, { replace: true });
}

// loadPeople fetches the active page from the server. Sort + offset
// are URL-driven, so the URL is the single source of truth.
function loadPeople() {
  const p = state.people;
  p.loading = true;
  p.error = null;
  const params = new URLSearchParams();
  params.set('limit', String(p.limit));
  params.set('offset', String(p.offset));
  params.set('sort', p.sortKey);
  params.set('dir', p.sortDir);
  return api.get('/people?' + params.toString()).then((body) => {
    p.items = (body && body.items) || [];
    p.total = (body && body.total) || 0;
    p.loading = false;
  }).catch((err) => {
    p.error = err;
    p.loading = false;
  });
}

// setQuery updates the search query + URL but does NOT reset offset
// or refetch — search filters the current page client-side for
// instant feedback. The user pages forward with Next » to search the
// rest of the list.
function setQuery(q) {
  state.people.q = q || '';
  pushURLParams();
}

// setSort toggles direction when the active column is clicked, else
// drops to the column's default direction (count desc, otherwise asc).
// Resets to page 1 since the row at offset N changes meaning when the
// sort axis flips.
function setSort(key) {
  const p = state.people;
  if (p.sortKey === key) {
    p.sortDir = p.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    p.sortKey = key;
    p.sortDir = key === 'count' ? 'desc' : 'asc';
  }
  p.offset = 0;
  pushURLParams();
  loadPeople();
}

// setOffset is the Pagination component's callback. Updates state +
// URL + refetch.
function setOffset(newOffset) {
  state.people.offset = newOffset;
  pushURLParams();
  loadPeople();
}

// filterItems narrows the list by case-insensitive name substring.
function filterItems(items, q) {
  const needle = String(q || '').trim().toLowerCase();
  if (!needle) return items.slice();
  return items.filter((it) => (it.name || '').toLowerCase().includes(needle));
}

// AvatarPlaceholder renders DaisyUI's avatar-placeholder with the
// performer's initials. Used for every row on the list page since the
// list endpoint doesn't ship headshot URLs — see comment at top.
function AvatarPlaceholder(name, sizeClass) {
  return m('div', { class: 'avatar avatar-placeholder' },
    m('div', {
      class: 'bg-neutral text-neutral-content rounded-full ' + sizeClass,
    }, m('span', { class: 'text-sm' }, monogram(name))),
  );
}

// HeaderCell renders one sortable <th>, mirroring the Library.js
// pattern (caret + aria-sort + keyboard activation).
function HeaderCell(col, sortKey, sortDir, extraClass) {
  const isActive = col.key === sortKey;
  let caret = '';
  if (isActive) caret = sortDir === 'desc' ? ' ▼' : ' ▲';
  const ariaSort = isActive
    ? (sortDir === 'desc' ? 'descending' : 'ascending')
    : 'none';
  return m('th', {
    class: 'cursor-pointer select-none' + (extraClass ? ' ' + extraClass : ''),
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

// StateBadgeCluster renders one small badge per non-zero state in
// STATE_ORDER. Empty when state_counts is missing or all zero so the
// cell collapses cleanly.
function StateBadgeCluster(stateCounts) {
  const counts = stateCounts || {};
  const cells = STATE_ORDER
    .filter((key) => (counts[key] || 0) > 0)
    .map((key) => {
      const meta = STATUS_META[key];
      return m('span', {
        class: 'badge badge-sm ' + meta.badge,
        title: meta.label + ': ' + counts[key],
      }, counts[key] + ' ' + meta.short);
    });
  if (cells.length === 0) return m('span', { class: 'opacity-50' }, '—');
  return m('div', { class: 'flex flex-wrap gap-1' }, cells);
}

// Row renders one performer.
function Row(it) {
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/people/' + it.performer_id),
  }, [
    m('td', { class: 'w-12' }, AvatarPlaceholder(it.name, 'w-10')),
    m('td', [
      m('div', { class: 'font-medium' }, it.name || 'Unknown'),
      m('div', { class: 'text-xs opacity-60' }, 'p-' + it.performer_id),
    ]),
    m('td', { class: 'font-mono text-sm text-right' },
      String(it.recording_count || 0)),
    m('td', StateBadgeCluster(it.state_counts)),
  ]);
}

// SearchIcon is the inline search SVG from DaisyUI's
// search-input-with-icon snippet.
function SearchIcon() {
  return m('svg', {
    class: 'h-[1em] opacity-50',
    xmlns: 'http://www.w3.org/2000/svg',
    viewBox: '0 0 24 24',
  }, m('g', {
    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    'stroke-width': '2.5', fill: 'none', stroke: 'currentColor',
  }, [
    m('circle', { cx: 11, cy: 11, r: 8 }),
    m('path', { d: 'm21 21-4.3-4.3' }),
  ]));
}

const People = {
  oninit() {
    readURLParams();
    loadPeople();
  },

  // onupdate fires on every route change while we stay mounted. Like
  // Library.js, the SPA keeps People mounted whenever the route stays
  // /people, so re-read the querystring to pick up search/sort changes.
  onupdate() {
    readURLParams();
  },

  view() {
    const p = state.people;

    if (p.loading && p.items.length === 0) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading people…');
    }
    if (p.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load people: ' + (p.error.message || p.error)),
      ]);
    }

    // Search filters the CURRENT page only — see header comment.
    const filtered = filterItems(p.items, p.q);
    const total = p.total;
    const showing = filtered.length;
    const start = total === 0 ? 0 : p.offset + 1;
    const end = Math.min(p.offset + p.limit, total);

    let subText;
    if (p.q) {
      subText = showing + ' on this page match "' + p.q + '" · ' +
        total + ' performers in your library';
    } else {
      subText = 'Showing ' + start + '–' + end + ' of ' + total +
        ' performer' + (total === 1 ? '' : 's') + ' in your library';
    }

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'People'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        // Action stub — kept for visual parity with Library.
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', { type: 'button', class: 'btn btn-ghost btn-sm', disabled: true }, 'Filters'),
        ]),
      ]),

      // Search row. DaisyUI's search-input-with-icon snippet wraps an
      // <input type="search"> inside a <label class="input">.
      m('div', { class: 'max-w-md' },
        m('label', { class: 'input w-full' }, [
          SearchIcon(),
          m('input', {
            type: 'search',
            placeholder: 'Search performers (current page)',
            value: p.q,
            oninput: (ev) => setQuery(ev.target.value),
          }),
        ])),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table table-zebra' }, [
          m('thead', m('tr', [
            m('th', { class: 'w-12', 'aria-hidden': 'true' }),
            HeaderCell(SORT_COLUMNS[0], p.sortKey, p.sortDir),
            HeaderCell(SORT_COLUMNS[1], p.sortKey, p.sortDir, 'text-right'),
            m('th', 'States'),
          ])),
          m('tbody', filtered.length === 0
            ? m('tr', m('td', {
                colspan: 4, class: 'text-center opacity-60 py-8',
              }, p.q
                ? 'No performers on this page match this search.'
                : 'No performers in your library yet.'))
            : filtered.map(Row)),
        ])),

      // Pagination strip — hidden when the entire library fits on
      // one page. Search doesn't constrain the slice the server
      // returned, so paging stays driven by total / limit / offset.
      m(Pagination, {
        offset: p.offset,
        limit: p.limit,
        total: p.total,
        setOffset,
      }),
    ]);
  },
};

export default People;
