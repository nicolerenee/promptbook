// Mismatches.js — Mithril port of the legacy /static/mismatches.js.
//
// Surfaces every divergence between the local cache and Encora's view
// of the user's collection / wants. Tabs filter by mismatch type; rows
// expose checkboxes (for actionable types only) and a primary "Push"
// button at the page level that POSTs to /api/v1/apply with the
// selected actions. The legacy page used a per-page batch-apply (Push
// N to Encora) — this port keeps that workflow rather than per-row
// CTAs because the selection model is the legacy file's primary
// interaction.
//
// API payload field names mirror the JSON shape of MismatchItem in
// internal/server/mismatch.go: `type` (not `mismatch_type`),
// `recording_id`, `show`, `tour`, `description`, `local_format`,
// `encora_format`. The /api/v1/apply body is the same shape the
// legacy file used: { actions: [{ type, recording_id, new_format }] }.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';

// TYPE_META maps each mismatch type to display + behavior metadata.
// `actionable: false` means the user has to acquire the file first —
// there's nothing to push for missing/wanted file rows, so their
// checkboxes render disabled and they're skipped from any apply
// payload. Badge classes follow the design-doc taxonomy:
//   add_to_collection → info
//   format_mismatch   → warning
//   missing_file      → error
//   wanted_file       → info
const TYPE_META = {
  add_to_collection: {
    label: 'Add to collection',
    pill: 'Wanted',
    badge: 'badge-info',
    resolution: 'Push to Encora',
    actionable: true,
  },
  format_mismatch: {
    label: 'Format mismatch',
    pill: 'Mismatch',
    badge: 'badge-warning',
    resolution: 'Update format on Encora',
    actionable: true,
  },
  missing_file: {
    label: 'Missing file',
    pill: 'Missing',
    badge: 'badge-error',
    resolution: 'Download from trader',
    actionable: false,
  },
  wanted_file: {
    label: 'Wanted file',
    pill: 'Wanted',
    badge: 'badge-info',
    resolution: 'Download from trader',
    actionable: false,
  },
};

// TYPE_FILTERS lists the filter tabs in display order. Empty key = All.
const TYPE_FILTERS = [
  { key: '',                  label: 'All' },
  { key: 'add_to_collection', label: 'Add to collection' },
  { key: 'format_mismatch',   label: 'Format mismatch' },
  { key: 'missing_file',      label: 'Missing file' },
  { key: 'wanted_file',       label: 'Wanted file' },
];

// countByType tallies items per mismatch type for the tab badges. The
// '' key holds the total so the All tab gets its count without a
// special case.
function countByType(items) {
  const out = { '': items.length };
  TYPE_FILTERS.forEach((f) => { if (f.key) out[f.key] = 0; });
  items.forEach((it) => {
    if (out[it.type] != null) out[it.type]++;
  });
  return out;
}

// readURLParams pulls ?type=… off the current Mithril route into the
// shared state slot so a reload or share keeps the same filter.
function readURLParams() {
  const params = m.route.param() || {};
  const raw = (params.type || '').toLowerCase();
  const canonical = TYPE_FILTERS.find((f) => f.key.toLowerCase() === raw);
  state.mismatches.type = canonical ? canonical.key : '';
}

// pushURLParams syncs state.mismatches.type back to the URL. Empty
// param drops the key entirely so /mismatches stays clean for the All
// tab.
function pushURLParams() {
  const out = {};
  if (state.mismatches.type) out.type = state.mismatches.type;
  m.route.set('/mismatches', out, { replace: true });
}

function setActiveType(key) {
  state.mismatches.type = key;
  pushURLParams();
}

// actionableSelectedCount tallies how many of the currently-selected
// recording ids are actionable. Disabled (missing/wanted) rows can't
// be checked in the first place, but a stale Set could carry an id
// whose row flipped types under us — gate on the live item.
function actionableSelectedCount() {
  const mm = state.mismatches;
  const byID = {};
  mm.items.forEach((it) => { byID[it.recording_id] = it; });
  let n = 0;
  mm.selected.forEach((id) => {
    const it = byID[id];
    if (it && TYPE_META[it.type] && TYPE_META[it.type].actionable) n++;
  });
  return n;
}

// fetchMismatches refreshes the items list + index and prunes any
// stale selections (rows that disappeared between fetches).
function fetchMismatches() {
  const mm = state.mismatches;
  mm.loading = true;
  mm.error = null;
  return api.get('/mismatches').then((body) => {
    mm.items = (body && body.items) || [];
    const live = new Set(mm.items.map((it) => it.recording_id));
    const stale = [];
    mm.selected.forEach((id) => { if (!live.has(id)) stale.push(id); });
    stale.forEach((id) => { mm.selected.delete(id); });
    mm.loading = false;
  }).catch((err) => {
    mm.error = err;
    mm.loading = false;
  });
}

// onPush fires the batch apply. Builds the actions array per the
// legacy contract — only actionable types contribute, and
// format_mismatch carries the local_format as new_format so the
// server's validation (which compares submitted new_format against the
// recording's current local_format) accepts the push.
function onPush() {
  const mm = state.mismatches;
  const actions = [];
  const byID = {};
  mm.items.forEach((it) => { byID[it.recording_id] = it; });
  mm.selected.forEach((id) => {
    const it = byID[id];
    if (!it) return;
    const meta = TYPE_META[it.type];
    if (!meta || !meta.actionable) return;
    const act = {
      type: it.type,
      recording_id: it.recording_id,
      new_format: '',
    };
    if (it.type === 'format_mismatch') {
      act.new_format = it.local_format || '';
    }
    actions.push(act);
  });
  if (actions.length === 0) return;

  const msg = 'Push ' + actions.length + ' change' +
    (actions.length === 1 ? '' : 's') + ' to Encora? This is irreversible.';
  // window.confirm is a deliberate hard stop matching the legacy
  // page — pushes are irreversible and we want the keystroke gate.
  // eslint-disable-next-line no-alert
  if (!window.confirm(msg)) return;

  mm.applying = true;
  mm.applyError = null;
  api.post('/apply', { actions }).then((body) => {
    mm.results = (body && body.results) || [];
    mm.selected.clear();
    mm.applying = false;
    // Reload the live list so successful pushes drop out and any
    // stale state shows up in subsequent batches.
    fetchMismatches();
  }).catch((err) => {
    mm.applyError = err;
    mm.applying = false;
  });
}

// ApplyResultsView renders the post-apply summary table once the user
// has fired off a batch. The Reload affordance clears it and returns
// to the live mismatch list.
function ApplyResultsView() {
  const mm = state.mismatches;
  const results = mm.results || [];
  const ok = results.reduce((n, r) => n + (r.ok ? 1 : 0), 0);

  return m('div', { class: 'space-y-4' }, [
    m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
      m('div', [
        m('h1', { class: 'text-3xl font-semibold' }, 'Mismatches'),
        m('p', { class: 'text-sm opacity-70 mt-1' },
          ok + ' of ' + results.length + ' action(s) succeeded'),
      ]),
      m('div', { class: 'flex items-center gap-2' }, [
        m('button', {
          type: 'button',
          class: 'btn btn-sm',
          onclick: () => {
            mm.results = null;
            mm.applyError = null;
            fetchMismatches();
          },
        }, 'Reload'),
        m('a', {
          class: 'btn btn-ghost btn-sm',
          href: '#',
          onclick: (ev) => { ev.preventDefault(); m.route.set('/history'); },
        }, 'View history'),
      ]),
    ]),
    m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
      m('table', { class: 'table table-zebra' }, [
        m('thead', m('tr', [
          m('th', 'Status'),
          m('th', 'Type'),
          m('th', 'Recording'),
          m('th', 'Detail'),
        ])),
        m('tbody', results.length === 0
          ? m('tr', m('td', {
              colspan: 4, class: 'text-center opacity-60 py-8',
            }, 'No actions submitted.'))
          : results.map((r) => {
            const act = r.action || {};
            const meta = TYPE_META[act.type] || {};
            const okBadge = r.ok ? 'badge-success' : 'badge-error';
            const okLabel = r.ok ? 'OK' : 'Error';
            let detail;
            if (r.ok) {
              detail = act.new_format
                ? m('span', { class: 'font-mono text-sm' },
                    'format → ' + act.new_format)
                : m('span', { class: 'opacity-60' }, 'pushed');
            } else {
              detail = m('span', { class: 'opacity-60' }, r.error || '');
            }
            return m('tr', [
              m('td', m('span', { class: 'badge ' + okBadge }, okLabel)),
              m('td', m('span', { class: 'badge ' + (meta.badge || 'badge-neutral') },
                act.type || '')),
              m('td', m('a', {
                class: 'link link-primary',
                href: '#',
                onclick: (ev) => {
                  ev.preventDefault();
                  m.route.set('/recordings/' + act.recording_id);
                },
              }, String(act.recording_id || ''))),
              m('td', detail),
            ]);
          })),
      ])),
  ]);
}

// Tab renders one filter chip in the DaisyUI tabs-box. Counts come
// from countByType; the active tab gets tab-active.
function Tab(filter, counts, active) {
  const isActive = filter.key === active;
  return m('a', {
    role: 'tab',
    class: 'tab' + (isActive ? ' tab-active' : ''),
    'aria-current': isActive ? 'page' : undefined,
    href: '#',
    onclick: (ev) => { ev.preventDefault(); setActiveType(filter.key); },
  }, [
    m('span', filter.label),
    m('span', { class: 'badge badge-sm badge-ghost ml-2' }, counts[filter.key] || 0),
  ]);
}

// Row renders one mismatch row. Actionable rows expose a checkbox the
// user toggles to add/remove from the selected Set; non-actionable
// rows render a disabled checkbox with a tooltip explaining why.
function Row(it) {
  const mm = state.mismatches;
  const meta = TYPE_META[it.type] || TYPE_META.add_to_collection;
  const isSelected = mm.selected.has(it.recording_id);
  const subtitle = (it.tour ? it.tour + ' · ' : '') + 'enc-' + it.recording_id;

  const checkbox = meta.actionable
    ? m('input', {
        type: 'checkbox',
        class: 'checkbox checkbox-sm',
        checked: isSelected,
        onclick: (ev) => { ev.stopPropagation(); },
        onchange: (ev) => {
          if (ev.target.checked) mm.selected.add(it.recording_id);
          else                   mm.selected.delete(it.recording_id);
        },
      })
    : m('input', {
        type: 'checkbox',
        class: 'checkbox checkbox-sm',
        disabled: true,
        title: 'Acquire the file separately to resolve this mismatch',
      });

  const detail = m('span', { class: 'font-mono text-xs' }, [
    it.description || '',
    !meta.actionable
      ? m('span', { class: 'opacity-60 ml-2' }, '(needs download)')
      : null,
  ]);

  return m('tr', {
    class: isSelected ? 'bg-base-300/40' : '',
  }, [
    m('td', { onclick: (ev) => ev.stopPropagation() }, checkbox),
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.pill)),
    m('td', {
      class: 'cursor-pointer',
      onclick: () => m.route.set('/recordings/' + it.recording_id),
    }, [
      m('div', { class: 'font-medium' }, it.show || '—'),
      m('div', { class: 'text-xs opacity-60' }, subtitle),
    ]),
    m('td', detail),
    m('td', { class: 'text-right text-sm opacity-80' }, '→ ' + meta.resolution),
  ]);
}

const Mismatches = {
  oninit() {
    readURLParams();
    // Drop any prior apply-result view when navigating back to the
    // page so a fresh visit always starts on the live mismatch list.
    state.mismatches.results = null;
    state.mismatches.applyError = null;
    fetchMismatches();
  },

  onupdate() {
    readURLParams();
  },

  view() {
    const mm = state.mismatches;

    if (mm.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading mismatches…');
    }
    if (mm.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load mismatches: ' +
          (mm.error.message || mm.error)),
      ]);
    }

    if (mm.results !== null) {
      return ApplyResultsView();
    }

    const items = mm.items;
    const counts = countByType(items);
    const active = mm.type;
    const filtered = active
      ? items.filter((it) => it.type === active)
      : items.slice();

    const selectedCount = actionableSelectedCount();
    const pushDisabled = selectedCount === 0 || mm.applying;

    return m('div', { class: 'space-y-6' }, [
      // Page header.
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Mismatches'),
          m('p', { class: 'text-sm opacity-70 mt-1' },
            'Differences between your library and Encora · ' +
            items.length + ' pending'),
        ]),
        m('div', { class: 'flex items-center gap-2' }, [
          m('span', { class: 'text-sm opacity-60 font-mono' },
            selectedCount + ' selected'),
          m('button', {
            type: 'button',
            class: 'btn btn-ghost btn-sm',
            disabled: mm.selected.size === 0 || mm.applying,
            onclick: () => { mm.selected.clear(); },
          }, 'Skip selected'),
          m('button', {
            type: 'button',
            class: 'btn btn-primary btn-sm',
            disabled: pushDisabled,
            onclick: onPush,
          }, mm.applying
            ? 'Pushing…'
            : 'Push ' + selectedCount + ' to Encora'),
        ]),
      ]),

      // Apply error banner — surfaces a network/server failure from
      // POST /apply without dropping the user back to the loading state.
      mm.applyError
        ? m('div', { role: 'alert', class: 'alert alert-error' }, [
            m('span', 'Apply failed: ' +
              (mm.applyError.message || mm.applyError)),
          ])
        : null,

      // Reconcile callout — sets expectations for what the buttons do.
      items.length > 0
        ? m('div', { role: 'alert', class: 'alert alert-info' }, [
            m('div', [
              m('h3', { class: 'font-bold' }, 'Reconciling local disk with Encora'),
              m('div', { class: 'text-xs' },
                'Promptbook is the source of truth for what’s on disk; ' +
                'Encora is the source of truth for what recordings exist. ' +
                'These items differ — pick which to push.'),
            ]),
          ])
        : null,

      // Filter tabs — same tabs-box pattern as Library.
      items.length > 0
        ? m('div', { role: 'tablist', class: 'tabs tabs-box' },
            TYPE_FILTERS.map((f) => Tab(f, counts, active)))
        : null,

      // Table or empty state.
      items.length === 0
        ? m('div', { class: 'p-8 text-center opacity-60' },
            'Everything synced. Encora and your library agree.')
        : m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
            m('table', { class: 'table table-zebra' }, [
              m('thead', m('tr', [
                m('th', { style: 'width:32px' }, ''),
                m('th', 'Type'),
                m('th', 'Recording'),
                m('th', 'Detail'),
                m('th', { class: 'text-right' }, 'Resolution'),
              ])),
              m('tbody', filtered.length === 0
                ? m('tr', m('td', {
                    colspan: 5, class: 'text-center opacity-60 py-8',
                  }, 'No mismatches match this filter.'))
                : filtered.map(Row)),
            ])),
    ]);
  },
};

export default Mismatches;
