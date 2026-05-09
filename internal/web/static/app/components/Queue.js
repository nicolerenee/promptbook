// Queue.js — Mithril port of the legacy /static/queue.js.
//
// Renders the manual import queue (files dropped into
// library.incomingDirs awaiting ingest). High-confidence rows expose a
// `Match` button that POSTs /api/v1/queue/{id}/import; lower-confidence
// rows show a disabled `Resolve…` stub.
//
// Behavior parity notes:
//   - Re-scan + Import-all-auto-resolved are cosmetic stubs (legacy
//     behavior). They stay disabled until a future wave wires them.
//   - On 503 from /import we surface the config-hint message; on 404 we
//     drop the row optimistically (the entry was already imported by a
//     parallel scan); other errors re-enable the button + alert.
//   - Items are sorted newest-first by discovered_at, matching the
//     legacy display order.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { humanSize, relativeTime, errorMessage } from '../utils/format.js';

// CONF_META keys on the lowercase API tokens. high → success (auto),
// medium → warning, low → error. Maps to DaisyUI badge color classes
// per the design doc.
const CONF_META = {
  high:   { label: 'High',   badge: 'badge-success' },
  medium: { label: 'Medium', badge: 'badge-warning' },
  low:    { label: 'Low',    badge: 'badge-error' },
};

// loadQueue fetches the queue list and stores it in shared state. Items
// are sorted newest-first so the most recent drops show at the top.
function loadQueue() {
  const q = state.queue;
  q.loading = true;
  q.error = null;
  return api.get('/queue').then((body) => {
    const items = (body && body.items) || [];
    items.sort((a, b) =>
      Date.parse(b.discovered_at || 0) - Date.parse(a.discovered_at || 0));
    q.items = items;
    q.loading = false;
  }).catch((err) => {
    q.error = err;
    q.loading = false;
  });
}

// countMetrics tallies the three header tiles. Anything that isn't a
// `high` confidence match counts as "needs you".
function countMetrics(items) {
  const out = { discovered: items.length, autoResolvable: 0, needsYou: 0 };
  items.forEach((it) => {
    if (it.suggested_confidence === 'high') out.autoResolvable++;
    else out.needsYou++;
  });
  return out;
}

// removeRow drops the queue entry with the supplied id from local state
// after a successful (or 404) import. Mithril's auto-redraw triggers on
// the next mutation cycle so the table re-renders without it.
function removeRow(queueID) {
  state.queue.items = state.queue.items.filter((it) =>
    String(it.id) !== String(queueID));
}

// handleImport posts to /api/v1/queue/:id/import after a confirm
// dialog. Mirrors the legacy queue.js handler's status-code branching.
function handleImport(item) {
  const q = state.queue;
  const id = item.id;
  const path = item.file_path || '';
  const suggested = item.suggested_recording_id || '';
  const msg = 'Import ' + path + ' as recording enc-' + suggested + '?';
  if (!window.confirm(msg)) return;

  q.importing[id] = true;
  m.redraw();

  api.post('/queue/' + encodeURIComponent(id) + '/import', {})
    .then((resp) => {
      if (resp && resp.ok) {
        delete q.importing[id];
        removeRow(id);
        return;
      }
      delete q.importing[id];
      window.alert('Import failed: ' + ((resp && resp.error) || 'unknown error'));
      m.redraw();
    })
    .catch((err) => {
      delete q.importing[id];
      // 503 (engine unconfigured) and 404 (queue entry already gone)
      // get bespoke handling per the legacy file.
      if (err && err.status === 503) {
        window.alert('Queue import is disabled — set library.root and ' +
          'PROMPTBOOK_ENCORA_APIKEY in config to enable.');
        m.redraw();
        return;
      }
      if (err && err.status === 404) {
        removeRow(id);
        return;
      }
      window.alert('Import failed: ' + errorMessage(err));
      m.redraw();
    });
}

// MetricTile renders one DaisyUI `stat` block. Mirrors the helper from
// Library.js, but kept local so each page can tune its tone classes
// without leaking visual tokens through utils/.
function MetricTile(label, num, sub, valueClass) {
  return m('div', { class: 'stat' }, [
    m('div', { class: 'stat-title' }, label),
    m('div', { class: 'stat-value text-2xl' + (valueClass ? ' ' + valueClass : '') },
      String(num)),
    m('div', { class: 'stat-desc' }, sub),
  ]);
}

// ConfidenceBadge renders the colored chip for a row's suggested
// confidence, or an em-dash placeholder when the field is empty.
function ConfidenceBadge(conf) {
  const meta = CONF_META[conf];
  if (!meta) return m('span', { class: 'opacity-60' }, '—');
  return m('span', { class: 'badge ' + meta.badge }, meta.label);
}

// SuggestedMatchCell renders the "Suggested match" column. When a
// recording id is suggested we link to /recordings/:id via the SPA
// router; otherwise show a muted placeholder.
function SuggestedMatchCell(item) {
  if (item.suggested_recording_id) {
    const href = '/recordings/' + item.suggested_recording_id;
    return m('a', {
      class: 'link link-hover font-mono text-sm',
      href,
      onclick: (ev) => { ev.preventDefault(); m.route.set(href); },
    }, 'enc-' + item.suggested_recording_id);
  }
  return m('span', { class: 'font-mono text-sm opacity-60' }, '— pick a recording —');
}

// ActionButton emits the right-aligned button for a queue row. High-
// confidence + suggested id → live `Match` button. Anything else →
// disabled `Resolve…` stub.
function ActionButton(item) {
  const importing = !!state.queue.importing[item.id];
  if (item.suggested_confidence === 'high' && item.suggested_recording_id) {
    if (importing) {
      return m('button', {
        type: 'button',
        class: 'btn btn-primary btn-sm',
        disabled: true,
      }, [
        m('span', { class: 'loading loading-spinner loading-xs' }),
        'Importing…',
      ]);
    }
    return m('button', {
      type: 'button',
      class: 'btn btn-primary btn-sm',
      onclick: () => handleImport(item),
    }, 'Match');
  }
  return m('button', {
    type: 'button',
    class: 'btn btn-sm',
    title: '(coming soon)',
    disabled: true,
  }, 'Resolve…');
}

// Row renders one queue entry. Path is rendered with truncate +
// title=full-path so long paths don't blow out the cell while still
// being inspectable on hover.
function Row(item) {
  return m('tr', { key: item.id }, [
    m('td',
      m('input', { type: 'checkbox', class: 'checkbox checkbox-sm',
        disabled: true, title: '(coming soon)' })),
    m('td', { class: 'font-mono text-sm whitespace-nowrap' },
      relativeTime(item.discovered_at)),
    m('td', ConfidenceBadge(item.suggested_confidence)),
    m('td',
      m('span', {
        class: 'font-mono text-sm block truncate max-w-[480px]',
        title: item.file_path || '',
      }, item.file_path || '')),
    m('td', SuggestedMatchCell(item)),
    m('td', { class: 'font-mono text-sm whitespace-nowrap' },
      humanSize(item.file_size_bytes)),
    m('td', { class: 'text-right' }, ActionButton(item)),
  ]);
}

const Queue = {
  oninit() { loadQueue(); },

  view() {
    const q = state.queue;
    const items = q.items;

    if (q.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading queue…');
    }
    if (q.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load queue: ' + errorMessage(q.error)),
      ]);
    }

    const metrics = countMetrics(items);
    const hasAuto = metrics.autoResolvable > 0;
    const subText =
      'Watching incoming · polled every minute · ' +
      items.length + ' entr' + (items.length === 1 ? 'y' : 'ies');

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Queue'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, subText),
        ]),
        // Action buttons — both cosmetic stubs (legacy behavior).
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: true,
            title: '(coming soon)',
          }, 'Re-scan'),
          m('button', {
            type: 'button',
            class: 'btn btn-primary btn-sm',
            disabled: !hasAuto,
            title: '(coming soon)',
          }, 'Import all auto-resolved'),
        ]),
      ]),

      // Metric tiles. text-success / text-warning tint the auto vs.
      // needs-you tiles to match the legacy pages' status colors.
      m('div', { class: 'stats stats-vertical lg:stats-horizontal shadow w-full' }, [
        MetricTile('Discovered',      metrics.discovered,      'in queue'),
        MetricTile('Auto-resolvable', metrics.autoResolvable,  'high-confidence match', 'text-success'),
        MetricTile('Needs you',       metrics.needsYou,        'awaiting review',       'text-warning'),
      ]),

      // Table.
      m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
        m('table', { class: 'table' }, [
          m('thead', m('tr', [
            m('th', { style: 'width:36px' }),
            m('th', 'Discovered'),
            m('th', 'Confidence'),
            m('th', 'File'),
            m('th', 'Suggested match'),
            m('th', 'Size'),
            m('th', { class: 'text-right', style: 'width:140px' }),
          ])),
          m('tbody', items.length === 0
            ? m('tr', m('td', {
                colspan: 7, class: 'text-center opacity-60 py-8',
              }, [
                'Queue is empty. Drop video files into your ',
                m('code', 'library.incomingDirs'),
                ' to see them here.',
              ]))
            : items.map(Row)),
        ])),

      // Footer hint about auto-import shortcuts.
      m('p', { class: 'text-xs opacity-60 italic' }, [
        'Files matching ', m('code', '[encora-NNNNN]'),
        ' in their name auto-import on the next scan. Drop a ',
        m('code', '.encora-id'),
        ' sidecar next to a video to add the ID without renaming.',
      ]),
    ]);
  },
};

export default Queue;
