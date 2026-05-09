// People.js — Mithril port of the legacy /static/people.js.
//
// Renders the /people index: page header w/ count, search input that
// filters client-side by name, and a sortable table of performers
// (avatar, name, recording count, per-state badge cluster). Clicking a
// row routes to /people/:id.
//
// Default sort is name ascending — matches the API's ORDER BY p.name
// and the legacy page's implicit ordering. The search box persists as
// ?q=<term> so deep links and refreshes keep the filter.
//
// Avatar fallback: /api/v1/people doesn't expose headshot_url on the
// list (only the detail endpoint does). We render DaisyUI's
// avatar-placeholder with the performer's initials in every row to
// keep the column shape consistent — the detail page is where we
// upgrade to a real headshot.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';

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

// SORT_COLUMNS lists the sortable headers in render order. Matches
// Library.js's pattern: `key` is both URL token + the column id;
// `compare` is a stable comparator.
const SORT_COLUMNS = [
  { key: 'name',  label: 'Performer',
    compare: (a, b) => cmpStr(a.name, b.name) },
  { key: 'count', label: 'Recordings',
    compare: (a, b) => cmpNum(a.recording_count, b.recording_count) },
];

const DEFAULT_SORT = { key: 'name', dir: 'asc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}
function cmpNum(a, b) { return (Number(a) || 0) - (Number(b) || 0); }

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

// readURLParams pulls the active search/sort state out of the current
// Mithril route. Empty params land on defaults.
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
}

// pushURLParams syncs state.people back to the browser URL so a reload
// or share keeps the same filtered/sorted view. Empty values are
// dropped.
function pushURLParams() {
  const p = state.people;
  const out = {};
  if (p.q) out.q = p.q;
  if (p.sortKey !== DEFAULT_SORT.key || p.sortDir !== DEFAULT_SORT.dir) {
    out.sort = p.sortKey;
    out.dir = p.sortDir;
  }
  m.route.set('/people', out, { replace: true });
}

// setQuery updates the search query and re-syncs the URL.
function setQuery(q) {
  state.people.q = q || '';
  pushURLParams();
}

// setSort toggles direction when the active column is clicked, else
// drops to the column's default direction (count desc, otherwise asc).
function setSort(key) {
  const p = state.people;
  if (p.sortKey === key) {
    p.sortDir = p.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    p.sortKey = key;
    p.sortDir = key === 'count' ? 'desc' : 'asc';
  }
  pushURLParams();
}

// filterItems narrows the list by case-insensitive name substring.
function filterItems(items, q) {
  const needle = String(q || '').trim().toLowerCase();
  if (!needle) return items.slice();
  return items.filter((it) => (it.name || '').toLowerCase().includes(needle));
}

// sortItems returns a new sorted slice. Direction is applied via the
// sign trick used in Library.js.
function sortItems(items, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => sign * col.compare(a, b));
  return out;
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
    state.people.loading = true;
    state.people.error = null;
    api.get('/people?limit=500').then((body) => {
      state.people.items = (body && body.items) || [];
      state.people.loading = false;
    }).catch((err) => {
      state.people.error = err;
      state.people.loading = false;
    });
  },

  // onupdate fires on every route change while we stay mounted. Like
  // Library.js, the SPA keeps People mounted whenever the route stays
  // /people, so re-read the querystring to pick up search/sort changes.
  onupdate() {
    readURLParams();
  },

  view() {
    const p = state.people;

    if (p.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading people…');
    }
    if (p.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load people: ' + (p.error.message || p.error)),
      ]);
    }

    const filtered = filterItems(p.items, p.q);
    const sorted = sortItems(filtered, { key: p.sortKey, dir: p.sortDir });
    const total = p.items.length;
    const showing = sorted.length;

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'People'),
          m('p', { class: 'text-sm opacity-70 mt-1' },
            (p.q
              ? showing + ' of ' + total + ' performers'
              : total + ' performer' + (total === 1 ? '' : 's')) +
            ' in your library'),
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
            placeholder: 'Search performers',
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
          m('tbody', sorted.length === 0
            ? m('tr', m('td', {
                colspan: 4, class: 'text-center opacity-60 py-8',
              }, p.q
                ? 'No performers match this search.'
                : 'No performers in your library yet.'))
            : sorted.map(Row)),
        ])),
    ]);
  },
};

export default People;
