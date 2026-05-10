// Sync.js — Mithril port of the legacy /static/sync.js.
//
// The Sync page is the activity log for Encora collection mirroring:
// each row is one /sync/runs entry with started/finished timestamps,
// duration, ok/error counts, and the rate-limit budget reported by
// the Encora API at the moment the run wrote the row.
//
// Visual structure:
//   - Page header: title + "last run X ago" sub-text + cosmetic
//     "Sync now" stub button (no SPA-driven sync trigger yet — the
//     button carries a tooltip pointing users at the CLI).
//   - DaisyUI `card` summary up top with Last run / Rate budget /
//     Errors-24h / Next run.
//   - Sortable table of the last 30 runs (Started, Duration, Status,
//     OK, Errors, Rate budget).
//
// Default sort: started_at desc, matching the API response order
// (id desc) and the natural "newest first" expectation. Clicks on
// other column headers re-sort with the same toggle behavior the
// Library + Wants pages use. URL persists ?sort=&dir=.

import m from 'https://esm.sh/mithril@2.2.2';
import graphql from '../graphql.js';
import state from '../state.js';
import { relativeTime } from '../utils/format.js';

// SYNC_RUNS_QUERY pulls the most recent SyncRun rows via the entgql
// `syncRuns` Relay connection. Limited to 30 rows to match the legacy
// REST cap (syncRunsListLimit). Field names are camelCase per gqlgen
// convention; mapSyncRun normalizes them back to the snake_case shape
// the legacy renderer was written against.
const SYNC_RUNS_QUERY = `
  query SyncRuns($first: Int!) {
    syncRuns(first: $first, orderBy: { field: STARTED_AT, direction: DESC }) {
      edges {
        node {
          id
          kind
          startedAt
          finishedAt
          okCount
          errorCount
          rateLimitRemaining
          errorText
        }
      }
    }
  }
`;

// SYNC_RUNS_LIMIT mirrors the server-side syncRunsListLimit constant;
// kept here so a future tweak only has to land in two places.
const SYNC_RUNS_LIMIT = 30;

// mapSyncRun rewrites a GraphQL SyncRun node into the snake_case shape
// the renderer + sort comparators expect. Time fields stay as
// RFC3339 strings (gqlgen's MarshalTime default) — Date.parse handles
// both that and the legacy "2006-01-02 15:04:05" format the REST
// endpoint used to emit, so the renderer keeps reading correctly.
function mapSyncRun(node) {
  if (!node) return null;
  return {
    id:                   node.id,
    kind:                 node.kind || '',
    started_at:           node.startedAt || '',
    finished_at:          node.finishedAt || null,
    ok_count:             node.okCount || 0,
    error_count:          node.errorCount || 0,
    rate_limit_remaining: node.rateLimitRemaining || 0,
    error_text:           node.errorText || '',
  };
}

// Encora is hard-capped at 30 requests/minute; the rate budget cell
// renders as remaining / RATE_CEILING.
const RATE_CEILING = 30;

// SORT_COLUMNS lists every sortable column in render order.
const SORT_COLUMNS = [
  { key: 'started_at',           label: 'Started',
    compare: (a, b) => cmpStr(a.started_at, b.started_at) },
  { key: 'duration',             label: 'Duration',
    compare: (a, b) => cmpNum(durationMs(a), durationMs(b)) },
  { key: 'status',               label: 'Status',
    compare: (a, b) => cmpStr(statusKey(a), statusKey(b)) },
  { key: 'ok_count',             label: 'OK',
    compare: (a, b) => cmpNum(a.ok_count, b.ok_count) },
  { key: 'error_count',          label: 'Errors',
    compare: (a, b) => cmpNum(a.error_count, b.error_count) },
  { key: 'rate_limit_remaining', label: 'Rate budget',
    compare: (a, b) => cmpNum(a.rate_limit_remaining, b.rate_limit_remaining) },
];

const DEFAULT_SORT = { key: 'started_at', dir: 'desc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}
function cmpNum(a, b) { return (Number(a) || 0) - (Number(b) || 0); }

// durationMs returns the elapsed ms for a run. Unfinished runs sort
// as 0 so they bunch with "no duration yet" — close enough for a
// table sort, and the cell still renders "running…" so users can tell.
function durationMs(run) {
  if (!run.started_at || !run.finished_at) return 0;
  const s = Date.parse(run.started_at);
  const f = Date.parse(run.finished_at);
  if (Number.isNaN(s) || Number.isNaN(f)) return 0;
  return Math.max(0, f - s);
}

// statusKey collapses a run's outcome to a sort-stable token. Mirrors
// the legacy resultBadge logic: zero errors → synced; some errors with
// any successes → retried; all errors → errored; in-flight → running.
function statusKey(run) {
  if (!run.finished_at) return 'running';
  const errs = run.error_count || 0;
  const ok = run.ok_count || 0;
  if (errs === 0) return 'synced';
  if (ok > 0)     return 'retried';
  return 'errored';
}

// statusBadge maps a run to a DaisyUI badge with the right color tone.
// Color mapping follows the design taxonomy — success/warning/error/info.
function statusBadge(run) {
  if (!run.finished_at) {
    return m('span', { class: 'badge badge-info' }, 'Running');
  }
  const errs = run.error_count || 0;
  const ok = run.ok_count || 0;
  if (errs === 0) {
    return m('span', { class: 'badge badge-success' }, 'Synced');
  }
  if (ok > 0) {
    return m('span', { class: 'badge badge-warning' }, 'Retried');
  }
  return m('span', { class: 'badge badge-error' }, 'Errored');
}

// formatDuration renders durationMs() output as "{X.X}s", or
// "running…" for unfinished runs, or "—" when the timestamps are
// missing/unparseable.
function formatDuration(run) {
  if (!run.started_at) return '—';
  if (!run.finished_at) return 'running…';
  const ms = durationMs(run);
  if (ms <= 0 && (run.started_at !== run.finished_at)) return '—';
  return (ms / 1000).toFixed(1) + 's';
}

// mostRecentFinish picks the latest finished_at across all runs. Runs
// in flight (finished_at == null) are skipped because we want "last
// completed run", not "last started run".
function mostRecentFinish(items) {
  let best = null;
  for (const it of items) {
    if (!it.finished_at) continue;
    const t = Date.parse(it.finished_at);
    if (Number.isNaN(t)) continue;
    if (best === null || t > best) best = t;
  }
  return best;
}

// errorsLast24h sums error_count across runs whose started_at is in
// the last 24 hours. Falls back to finished_at when started_at is
// missing — a freshly inserted in-flight run might have either.
function errorsLast24h(items) {
  const cutoff = Date.now() - 24 * 60 * 60 * 1000;
  let n = 0;
  for (const it of items) {
    const stamp = it.started_at || it.finished_at;
    if (!stamp) continue;
    const t = Date.parse(stamp);
    if (Number.isNaN(t)) continue;
    if (t >= cutoff) n += it.error_count || 0;
  }
  return n;
}

function readURLParams() {
  const params = m.route.param() || {};
  const s = state.sync;

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
}

function pushURLParams() {
  const s = state.sync;
  m.route.set('/sync', {
    sort: s.sortKey,
    dir:  s.sortDir,
  }, { replace: true });
}

function setSort(key) {
  const s = state.sync;
  if (s.sortKey === key) {
    s.sortDir = s.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    s.sortKey = key;
    // Numeric / chronological columns default to desc (newest /
    // largest first); textual ones default to asc.
    s.sortDir = (
      key === 'started_at' ||
      key === 'duration' ||
      key === 'ok_count' ||
      key === 'error_count' ||
      key === 'rate_limit_remaining'
    ) ? 'desc' : 'asc';
  }
  pushURLParams();
}

function sortItems(items, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return items.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = items.slice();
  out.sort((a, b) => sign * col.compare(a, b));
  return out;
}

// HeaderCell renders one sortable <th>. Mirrors Library's pattern.
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

// Row renders one sync run.
function Row(run) {
  const errs = run.error_count || 0;
  const ok = run.ok_count || 0;
  const rem = Math.max(0, Math.min(RATE_CEILING, run.rate_limit_remaining || 0));
  const pct = (rem / RATE_CEILING) * 100;
  const startedISO = run.started_at || '';
  const subTitle = (run.kind || 'all') + ' · #' +
    (run.id != null ? run.id : '?');

  return m('tr', { class: 'hover:bg-base-200' }, [
    m('td', { title: startedISO }, [
      m('div', { class: 'font-medium' }, relativeTime(startedISO)),
      m('div', { class: 'text-xs opacity-60 font-mono' }, subTitle),
    ]),
    m('td', { class: 'font-mono text-sm' }, formatDuration(run)),
    m('td', statusBadge(run)),
    m('td', { class: 'font-mono text-sm' }, String(ok)),
    m('td', {
      class: 'font-mono text-sm' + (errs > 0 ? ' text-error' : ''),
    }, String(errs)),
    m('td', m('div', { class: 'flex items-center gap-2' }, [
      m('progress', {
        class: 'progress w-20' + (
          errs > 0 ? ' progress-error' :
          pct >= 60 ? ' progress-success' :
          pct >= 20 ? ' progress-warning' : ' progress-error'
        ),
        value: rem,
        max: RATE_CEILING,
      }),
      m('span', { class: 'font-mono text-xs' }, rem + '/' + RATE_CEILING),
    ])),
  ]);
}

// SummaryCard renders the "last run" overview using DaisyUI card.
function SummaryCard(items) {
  const lastFinish = mostRecentFinish(items);
  const lastRunLabel = lastFinish
    ? relativeTime(new Date(lastFinish).toISOString())
    : 'never';
  const newest = items.length > 0 ? items[0] : null;
  const rateBudget = newest
    ? (newest.rate_limit_remaining + '/' + RATE_CEILING)
    : ('—/' + RATE_CEILING);
  const errs24 = errorsLast24h(items);

  function tile(label, value, sub, accent) {
    return m('div', { class: 'stat' }, [
      m('div', { class: 'stat-title' }, label),
      m('div', {
        class: 'stat-value text-2xl' + (accent ? ' text-error' : ''),
      }, String(value)),
      m('div', { class: 'stat-desc' }, sub),
    ]);
  }

  return m('div', { class: 'card bg-base-200 shadow' },
    m('div', { class: 'card-body p-4' }, [
      m('h2', { class: 'card-title text-base mb-2' }, 'Last run'),
      m('div', { class: 'stats stats-vertical lg:stats-horizontal w-full' }, [
        tile('Last run', lastRunLabel,
          newest ? (newest.kind || 'all') + ' · run #' + newest.id : 'no runs yet'),
        tile('Rate budget', rateBudget, 'remaining of 30/min ceiling'),
        tile('Errors 24h', errs24,
          errs24 > 0 ? 'across recent runs' : 'all clean', errs24 > 0),
        tile('Next run', 'manual', 'no scheduler configured'),
      ]),
    ]),
  );
}

const Sync = {
  oninit() {
    readURLParams();
    state.sync.loading = true;
    state.sync.error = null;
    graphql.query(SYNC_RUNS_QUERY, { first: SYNC_RUNS_LIMIT }).then((data) => {
      const edges = (data && data.syncRuns && data.syncRuns.edges) || [];
      state.sync.items = edges.map((e) => mapSyncRun(e && e.node)).filter(Boolean);
      state.sync.loading = false;
    }).catch((err) => {
      state.sync.error = err;
      state.sync.loading = false;
    });
  },

  onupdate() {
    readURLParams();
  },

  view() {
    const s = state.sync;
    const items = s.items;

    if (s.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading sync history…');
    }
    if (s.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load sync runs: ' + (s.error.message || s.error)),
      ]);
    }

    const sorted = sortItems(items, { key: s.sortKey, dir: s.sortDir });
    const lastFinish = mostRecentFinish(items);
    const subText = lastFinish
      ? 'Last run ' + relativeTime(new Date(lastFinish).toISOString()) +
        ' · ceiling 30 req/min'
      : 'No runs yet · ceiling 30 req/min';

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Sync'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', {
            type: 'button', class: 'btn btn-ghost btn-sm', disabled: true,
            title: 'Sync configuration is in promptbook.yaml for now.',
          }, 'Configure'),
          m('button', {
            type: 'button', class: 'btn btn-primary btn-sm', disabled: true,
            title: 'Use the CLI for now: promptbook collection sync',
          }, 'Sync now'),
        ]),
      ]),

      // Last-run summary card.
      SummaryCard(items),

      // Run history table or empty state.
      items.length === 0
        ? m('div', { class: 'rounded-box bg-base-200 p-8 text-center opacity-70' },
            'No sync runs yet. Run promptbook collection sync ' +
            'or promptbook serve to start mirroring.')
        : m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
            m('table', { class: 'table table-zebra' }, [
              m('thead', m('tr',
                SORT_COLUMNS.map((col) => HeaderCell(col, s.sortKey, s.sortDir)),
              )),
              m('tbody', sorted.map(Row)),
            ])),
    ]);
  },
};

export default Sync;
