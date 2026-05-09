// Jobs.js — scheduled-jobs dashboard. Mirrors the Radarr "System
// > Tasks" / "System > Queue" tables: one section listing every
// registered scheduled job with its interval and last/next run, a
// second section listing recent runs with their lifecycle state.
//
// Polling: a 5s setInterval refreshes both endpoints while the
// component is mounted; oncreate/onremove gate the timer so a route
// switch immediately stops the polling.
//
// Manual trigger: each scheduled-row carries a refresh-icon button
// that POSTs /jobs/scheduled/:name/run; on success we eagerly
// re-poll the queue so the new "queued" row shows up before the
// next 5s tick.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { relativeTime, errorMessage } from '../utils/format.js';

// POLL_MS is how often the page refreshes the two endpoints. 5s
// matches the design doc; faster would chatter the server, slower
// would feel laggy after clicking a manual trigger.
const POLL_MS = 5000;

// humanInterval renders a duration in the same shorthand Radarr's
// Tasks page uses: "15 min", "1 day", "30 sec". 0 means manual-only,
// and surfaces as "Manual" so users see why there's no Next column
// for that row.
function humanInterval(ms) {
  if (!ms || ms <= 0) return 'Manual';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + ' sec';
  const min = Math.round(s / 60);
  if (min < 60) return min + ' min';
  const hr = Math.round(min / 60);
  if (hr < 24) return hr + (hr === 1 ? ' hr' : ' hrs');
  const day = Math.round(hr / 24);
  return day + (day === 1 ? ' day' : ' days');
}

// formatDuration renders ms as mm:ss. Matches the Radarr screenshot
// reference; for runs longer than an hour we still pad to mm:ss so
// the column stays narrow (long runs are rare in practice).
function formatDuration(ms) {
  if (!ms || ms <= 0) return '00:00:00';
  const totalSec = Math.floor(ms / 1000);
  const hh = Math.floor(totalSec / 3600);
  const mm = Math.floor((totalSec % 3600) / 60);
  const ss = totalSec % 60;
  return [hh, mm, ss].map((n) => String(n).padStart(2, '0')).join(':');
}

// futureTime renders an upcoming timestamp as "in N min" / "in N hr"
// for the Next Execution column. Same granularity as relativeTime
// but inverted.
function futureTime(iso) {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '—';
  const sec = Math.round((t - Date.now()) / 1000);
  if (sec <= 0) return 'now';
  if (sec < 60) return 'in ' + sec + 's';
  const min = Math.round(sec / 60);
  if (min < 60) return 'in ' + min + 'm';
  const hr = Math.round(min / 60);
  if (hr < 24) return 'in ' + hr + 'h';
  const day = Math.round(hr / 24);
  return 'in ' + day + 'd';
}

// statusIcon returns a small inline SVG matching the run's lifecycle
// state. Mirrors the four-status taxonomy: queued (clock), running
// (spinner-style ring), succeeded (check), failed (x).
function statusIcon(status) {
  const base = {
    width: 16, height: 16, viewBox: '0 0 24 24',
    fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'aria-hidden': 'true',
  };
  switch (status) {
    case 'running':
      return m('svg', Object.assign({}, base, { class: 'text-info animate-spin' }), [
        m('path', { d: 'M21 12a9 9 0 1 1-6.2-8.55' }),
      ]);
    case 'succeeded':
      return m('svg', Object.assign({}, base, { class: 'text-success' }), [
        m('polyline', { points: '20 6 9 17 4 12' }),
      ]);
    case 'failed':
      return m('svg', Object.assign({}, base, { class: 'text-error' }), [
        m('line', { x1: 18, y1: 6, x2: 6, y2: 18 }),
        m('line', { x1: 6, y1: 6, x2: 18, y2: 18 }),
      ]);
    case 'queued':
    default:
      return m('svg', Object.assign({}, base, { class: 'opacity-70' }), [
        m('circle', { cx: 12, cy: 12, r: 9 }),
        m('polyline', { points: '12 7 12 12 16 14' }),
      ]);
  }
}

// refreshIcon is the click target on each scheduled row.
function refreshIcon() {
  return m('svg', {
    width: 16, height: 16, viewBox: '0 0 24 24',
    fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'aria-hidden': 'true',
  }, [
    m('polyline', { points: '23 4 23 10 17 10' }),
    m('polyline', { points: '1 20 1 14 7 14' }),
    m('path', { d: 'M3.51 9a9 9 0 0 1 14.85-3.36L23 10' }),
    m('path', { d: 'M20.49 15a9 9 0 0 1-14.85 3.36L1 14' }),
  ]);
}

// fetchAll re-pulls both endpoints and applies the result to state.
// Errors are stored on state.jobs.error so the page can surface them
// inline rather than silently going stale.
function fetchAll() {
  const j = state.jobs;
  return Promise.all([
    api.get('/jobs/scheduled'),
    api.get('/jobs/queue'),
  ]).then(([sch, q]) => {
    j.scheduled = (sch && sch.items) || [];
    j.queue = (q && q.items) || [];
    j.error = null;
    j.loading = false;
  }).catch((err) => {
    j.error = err;
    j.loading = false;
  });
}

// triggerRun POSTs the manual-trigger endpoint and re-polls so the
// new run shows up immediately. Per-row triggering state lets the
// button render disabled while the request is in flight.
function triggerRun(name) {
  const j = state.jobs;
  if (j.triggering[name]) return;
  j.triggering[name] = true;
  api.post('/jobs/scheduled/' + encodeURIComponent(name) + '/run', {})
    .then(() => fetchAll())
    .catch((err) => { j.triggerError = errorMessage(err); })
    .then(() => {
      j.triggering[name] = false;
      m.redraw();
    });
}

// ScheduledRow renders one /jobs/scheduled row.
function ScheduledRow(row) {
  const triggering = !!state.jobs.triggering[row.name];
  return m('tr', { class: 'hover:bg-base-200' }, [
    m('td', { class: 'font-medium' }, row.name),
    m('td', { class: 'font-mono text-sm' }, humanInterval(row.interval_ms)),
    m('td', { title: row.last_started_at || '' },
      relativeTime(row.last_started_at)),
    m('td', { class: 'font-mono text-sm' },
      formatDuration(row.last_duration_ms)),
    m('td', { title: row.next_run || '' },
      row.interval_ms > 0 ? futureTime(row.next_run) : '—'),
    m('td', { class: 'text-right' }, m('button', {
      type: 'button',
      class: 'btn btn-ghost btn-sm btn-square',
      title: 'Run now',
      'aria-label': 'Run ' + row.name + ' now',
      disabled: triggering,
      onclick: () => triggerRun(row.name),
    }, refreshIcon())),
  ]);
}

// formatArgs renders a JobArgs blob as a compact "key=value" chip
// label. Sorted keys for stability across redraws; longer values
// truncate at 24 chars so the column doesn't blow out. Returns null
// for empty / nullish args so the caller can drop the chip entirely.
const ARGS_VALUE_MAX = 24;
function formatArgs(args) {
  if (!args || typeof args !== 'object') return null;
  const keys = Object.keys(args);
  if (keys.length === 0) return null;
  keys.sort();
  return keys.map((k) => {
    let v = args[k];
    if (typeof v === 'string' && v.length > ARGS_VALUE_MAX) {
      v = v.slice(0, ARGS_VALUE_MAX - 1) + '…';
    } else if (v && typeof v === 'object') {
      v = JSON.stringify(v);
      if (v.length > ARGS_VALUE_MAX) v = v.slice(0, ARGS_VALUE_MAX - 1) + '…';
    }
    return k + '=' + v;
  }).join(' ');
}

// triggerLabel renders the row's trigger as a short, human-readable
// string. Mirrors the runner's Trigger constants but collapses the
// kebab-case "scheduled-fanout" into "fanout" for the table — the
// full word doesn't fit cleanly in the Detail column.
function triggerLabel(trigger) {
  switch (trigger) {
    case 'scheduled-fanout':
      return 'fanout';
    default:
      return trigger || '';
  }
}

// QueueRow renders one /jobs/queue row.
function QueueRow(row) {
  const errored = row.status === 'failed';
  const argsLabel = formatArgs(row.args);
  return m('tr', { class: 'hover:bg-base-200' }, [
    m('td', m('div', { class: 'flex flex-col gap-1' }, [
      m('div', { class: 'flex items-center gap-2' }, [
        statusIcon(row.status),
        m('span', { class: 'font-medium' }, row.job_name),
      ]),
      argsLabel ? m('div', {
        class: 'font-mono text-xs opacity-70 pl-6 truncate max-w-xs',
        title: argsLabel,
      }, argsLabel) : null,
    ])),
    m('td', { title: row.queued_at || '' }, relativeTime(row.queued_at)),
    m('td', { title: row.started_at || '' }, relativeTime(row.started_at)),
    m('td', { title: row.ended_at || '' }, relativeTime(row.ended_at)),
    m('td', { class: 'font-mono text-sm' }, formatDuration(row.duration_ms)),
    m('td', { class: 'text-xs' + (errored ? ' text-error' : ' opacity-60') },
      errored ? (row.error || 'failed') : triggerLabel(row.trigger)),
  ]);
}

const Jobs = {
  oninit() {
    state.jobs.loading = true;
    state.jobs.error = null;
    fetchAll();
  },

  oncreate() {
    state.jobs.timer = setInterval(() => {
      fetchAll().then(() => m.redraw());
    }, POLL_MS);
  },

  onremove() {
    if (state.jobs.timer) {
      clearInterval(state.jobs.timer);
      state.jobs.timer = null;
    }
  },

  view() {
    const j = state.jobs;
    if (j.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading jobs…');
    }
    if (j.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load jobs: ' + errorMessage(j.error)),
      ]);
    }

    const scheduled = j.scheduled || [];
    const queue = j.queue || [];

    return m('div', { class: 'space-y-8' }, [
      // Page header.
      m('header', [
        m('h1', { class: 'text-3xl font-semibold' }, 'Jobs'),
        m('p', { class: 'text-sm opacity-70 mt-1' },
          scheduled.length + ' scheduled · ' + queue.length + ' recent runs'),
      ]),

      // Trigger error (if any).
      j.triggerError ? m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', j.triggerError),
        m('button', {
          type: 'button', class: 'btn btn-sm btn-ghost ml-auto',
          onclick: () => { j.triggerError = null; },
        }, 'Dismiss'),
      ]) : null,

      // Scheduled section.
      m('section', { class: 'space-y-3' }, [
        m('h2', { class: 'text-lg font-semibold' }, 'Scheduled'),
        scheduled.length === 0
          ? m('div', { class: 'rounded-box bg-base-200 p-8 text-center opacity-70' },
              'No scheduled jobs registered.')
          : m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
              m('table', { class: 'table table-zebra' }, [
                m('thead', m('tr', [
                  m('th', 'Name'),
                  m('th', 'Interval'),
                  m('th', 'Last Execution'),
                  m('th', 'Last Duration'),
                  m('th', 'Next Execution'),
                  m('th', { class: 'w-16' }, ''),
                ])),
                m('tbody', scheduled.map(ScheduledRow)),
              ])),
      ]),

      // Queue section.
      m('section', { class: 'space-y-3' }, [
        m('h2', { class: 'text-lg font-semibold' }, 'Queue'),
        queue.length === 0
          ? m('div', { class: 'rounded-box bg-base-200 p-8 text-center opacity-70' },
              'No recent runs.')
          : m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
              m('table', { class: 'table table-zebra' }, [
                m('thead', m('tr', [
                  m('th', 'Job'),
                  m('th', 'Queued'),
                  m('th', 'Started'),
                  m('th', 'Ended'),
                  m('th', 'Duration'),
                  m('th', 'Detail'),
                ])),
                m('tbody', queue.map(QueueRow)),
              ])),
      ]),
    ]);
  },
};

export default Jobs;
