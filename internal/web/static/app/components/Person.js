// Person.js — Mithril port of the legacy /static/person.js.
//
// Renders /people/:id: page header w/ avatar + name + meta; stat tiles
// summarising the performer's library footprint (synced /
// wants+missing / total credits / years active); sortable table of
// recordings the performer appears in. Clicking a row routes to
// /recordings/:id.
//
// Avatar resolution: PersonDetail.HeadshotURL is best-effort. The
// server populates it when stagemedia returns a hit; we fall back to
// DaisyUI's avatar-placeholder rendering the performer's initials when
// it's empty (or the image fails to load — `onerror` flips state.error
// so the next render swaps to the placeholder).

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate } from '../utils/format.js';

// STATUS_META mirrors Library.js so per-recording badges read the same
// across pages.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// SORT_COLUMNS lists the appearances-table headers in render order.
const SORT_COLUMNS = [
  { key: 'status', label: 'Status',
    compare: (a, b) => cmpStr(a.state, b.state) },
  { key: 'show',   label: 'Show',
    compare: (a, b) => {
      let c = cmpStr(a.show, b.show); if (c) return c;
      return cmpStr(a.tour, b.tour);
    } },
  { key: 'date',   label: 'Date',
    compare: (a, b) => cmpStr(a.date_full, b.date_full) },
  { key: 'master', label: 'Master',
    // PersonRecording doesn't surface master in the API; sort kept for
    // signature parity with Library.js but compare is a stable noop.
    compare: () => 0 },
  { key: 'role',   label: 'Role',
    // role isn't on the API yet; same noop strategy as master.
    compare: () => 0 },
];

const DEFAULT_SORT = { key: 'date', dir: 'desc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}

// monogram returns up to 2 initials, matching People.js.
function monogram(name) {
  if (!name) return '?';
  const parts = String(name).trim().split(/\s+/);
  let letters = '';
  for (let i = 0; i < parts.length && letters.length < 2; i++) {
    if (parts[i].length > 0) letters += parts[i][0];
  }
  return letters.toUpperCase() || '?';
}

// readURLParams pulls the active sort state out of the route. The id
// itself is owned by Mithril path params (vnode.attrs.id) and lives on
// state.person.id.
function readURLParams() {
  const params = m.route.param() || {};
  const p = state.person;

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

// pushURLParams syncs sort state back to the URL while preserving
// :id. Empty sort lands on default — drop both keys to keep the URL
// clean.
function pushURLParams() {
  const p = state.person;
  if (!p.id) return;
  const out = {};
  if (p.sortKey !== DEFAULT_SORT.key || p.sortDir !== DEFAULT_SORT.dir) {
    out.sort = p.sortKey;
    out.dir = p.sortDir;
  }
  m.route.set('/people/:id', { id: p.id, ...out }, { replace: true });
}

// setSort toggles direction when the active column is clicked.
function setSort(key) {
  const p = state.person;
  if (p.sortKey === key) {
    p.sortDir = p.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    p.sortKey = key;
    p.sortDir = key === 'date' ? 'desc' : 'asc';
  }
  pushURLParams();
}

// sortItems returns a new sorted slice (Library.js's sign trick).
function sortItems(items, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => sign * col.compare(a, b));
  return out;
}

// stateCountsFromRecordings tallies State for each recording on the
// detail payload. Used to drive the stat tiles when the API doesn't
// echo a top-level state_counts.
function stateCountsFromRecordings(recs) {
  const counts = { synced: 0, format_mismatch: 0, missing: 0, wanted: 0, orphan: 0 };
  (recs || []).forEach((r) => {
    if (counts[r.state] != null) counts[r.state]++;
  });
  return counts;
}

// yearsActive returns "min–max" (en dash) across recording date_full
// values, single year if all recordings share one, or "—" if nothing
// parses. Mirrors legacy person.js.
function yearsActive(recs) {
  let minY = null;
  let maxY = null;
  (recs || []).forEach((r) => {
    if (!r.date_full) return;
    const y = parseInt(String(r.date_full).substring(0, 4), 10);
    if (!Number.isFinite(y)) return;
    if (minY === null || y < minY) minY = y;
    if (maxY === null || y > maxY) maxY = y;
  });
  if (minY === null) return '—';
  if (minY === maxY) return String(minY);
  return minY + '–' + maxY;
}

// Avatar renders an <img>-backed avatar when a headshot URL is
// available, falling back to DaisyUI's avatar-placeholder with the
// performer's initials. The img's `onerror` flips a flag on
// state.person so a broken URL retreats to the placeholder on the
// next render (rather than rendering a busted image icon).
function Avatar(detail, sizeClass) {
  const url = !state.person.imgError && detail.headshot_url ? detail.headshot_url : '';
  if (url) {
    return m('div', { class: 'avatar' },
      m('div', { class: 'rounded-full ' + sizeClass },
        m('img', {
          src: url,
          alt: detail.name || 'Performer',
          onerror: () => { state.person.imgError = true; m.redraw(); },
        }),
      ),
    );
  }
  return m('div', { class: 'avatar avatar-placeholder' },
    m('div', { class: 'bg-neutral text-neutral-content rounded-full ' + sizeClass },
      m('span', { class: 'text-3xl' }, monogram(detail.name)),
    ),
  );
}

// HeaderCell renders one sortable <th>, mirroring Library.js.
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

// Row renders one appearance.
function Row(r) {
  const meta = STATUS_META[r.state] || STATUS_META.orphan;
  const subtitle = (r.tour ? r.tour + ' · ' : '') + 'enc-' + r.id;
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + r.id),
  }, [
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
    m('td', [
      m('div', { class: 'font-medium' }, r.show || '—'),
      m('div', { class: 'text-xs opacity-60' }, subtitle),
    ]),
    m('td', { class: 'font-mono text-sm' },
      smartDate(r.date_full, r.date_month_known, r.date_day_known)),
    m('td', '—'),
    m('td', '—'),
  ]);
}

// MetricTile renders one DaisyUI stat block, lifted from Library.js.
function MetricTile(label, num, sub) {
  return m('div', { class: 'stat' }, [
    m('div', { class: 'stat-title' }, label),
    m('div', { class: 'stat-value text-2xl' }, String(num)),
    m('div', { class: 'stat-desc' }, sub),
  ]);
}

// loadDetail fetches /api/v1/people/:id and resets imgError so a
// fresh navigation always tries the headshot URL again.
function loadDetail(id) {
  state.person.id = id;
  state.person.detail = null;
  state.person.loading = true;
  state.person.error = null;
  state.person.imgError = false;
  api.get('/people/' + encodeURIComponent(id)).then((body) => {
    state.person.detail = body || null;
    state.person.loading = false;
  }).catch((err) => {
    state.person.error = err;
    state.person.loading = false;
  });
}

const Person = {
  oninit(vnode) {
    const id = String(vnode.attrs.id || '');
    readURLParams();
    if (id) loadDetail(id);
  },

  // onupdate handles two cases: (1) querystring sort change while we
  // stay on the same id; (2) :id change on a same-route navigation
  // (e.g. via a related-link). We compare against state.person.id and
  // re-fetch when it shifts.
  onupdate(vnode) {
    const id = String(vnode.attrs.id || '');
    if (id && id !== state.person.id) {
      loadDetail(id);
    }
    readURLParams();
  },

  view() {
    const p = state.person;

    if (p.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading performer…');
    }
    if (p.error) {
      const status = p.error.status;
      if (status === 404) {
        return m('div', { class: 'space-y-4' }, [
          m('div', { class: 'text-sm' },
            m('a', {
              class: 'link',
              href: '#',
              onclick: (ev) => { ev.preventDefault(); m.route.set('/people'); },
            }, '← People')),
          m('div', { role: 'alert', class: 'alert alert-warning' },
            m('span', 'No performer with that id is in your library.')),
        ]);
      }
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load performer: ' + (p.error.message || p.error)),
      ]);
    }

    const detail = p.detail;
    if (!detail) {
      return m('div', { class: 'p-8 opacity-60' }, 'No performer.');
    }

    const recordings = detail.recordings || [];
    const counts = stateCountsFromRecordings(recordings);
    const total = recordings.length;
    const synced = counts.synced;
    const wantsMissing = counts.wanted + counts.missing;
    const mismatch = counts.format_mismatch;
    const years = yearsActive(recordings);

    const sorted = sortItems(recordings, { key: p.sortKey, dir: p.sortDir });

    return m('div', { class: 'space-y-6' }, [
      // Breadcrumb-ish back link.
      m('div', { class: 'text-sm' },
        m('a', {
          class: 'link link-hover opacity-70',
          href: '#',
          onclick: (ev) => { ev.preventDefault(); m.route.set('/people'); },
        }, '← People')),

      // Hero: avatar + name + meta.
      m('header', { class: 'flex items-center gap-6 flex-wrap' }, [
        Avatar(detail, 'w-32'),
        m('div', { class: 'flex-1 min-w-0' }, [
          m('div', {
            class: 'text-xs uppercase tracking-wider opacity-60',
          }, 'Performer · p-' + detail.performer_id),
          m('h1', { class: 'text-3xl font-semibold mt-1' },
            detail.name || 'Unknown performer'),
          m('p', { class: 'text-sm opacity-70 mt-1' },
            total + ' recording' + (total === 1 ? '' : 's') +
            (synced ? ' · ' + synced + ' synced' : '') +
            (wantsMissing ? ' · ' + wantsMissing + ' wanted/missing' : '')),
        ]),
      ]),

      // Stat tiles.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Library credits', total,        'across your library'),
        MetricTile('Synced',          synced,       total + ' total'),
        MetricTile('Wants & missing', wantsMissing, 'on the shopping list'),
        MetricTile('Mismatches',      mismatch,     'needs reconcile'),
        MetricTile('Years active',    years,        'span of recordings'),
      ]),

      // Appearances table.
      m('h2', { class: 'text-sm font-semibold uppercase tracking-wider opacity-70' },
        'Appearances in your library'),
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table table-zebra' }, [
          m('thead', m('tr',
            SORT_COLUMNS.map((col) => HeaderCell(col, p.sortKey, p.sortDir)),
          )),
          m('tbody', sorted.length === 0
            ? m('tr', m('td', {
                colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
              }, 'No recordings of this performer in your library yet.'))
            : sorted.map(Row)),
        ])),
    ]);
  },
};

export default Person;
