// mismatches.js — fetches /api/v1/mismatches and renders the review +
// apply page. Selection state lives in a client-side Set keyed by
// recording id; the Push button POSTs the chosen actions to
// /api/v1/apply as JSON. Mirrors the API-only page pattern established
// by library.js — the server template ships an empty shell and this
// script populates the DOM.

(function () {
  'use strict';

  // TYPE_META describes each mismatch type: the filter-tab label,
  // pill class (reusing the .pb-status-* taxonomy so amber/violet/red
  // hues match the rest of the app), the dot color, the right-aligned
  // resolution string, and whether the row is actionable from this
  // page. missing_file / wanted_file aren't pushable — the user has
  // to acquire the file first — so they render with a disabled
  // checkbox and a muted "(needs download)" hint.
  var TYPE_META = {
    add_to_collection: {
      label:      'Add to collection',
      pill:       'Wanted',
      cls:        'pb-status-wanted',
      color:      'var(--status-wanted)',
      resolution: 'Push to Encora',
      actionable: true,
    },
    format_mismatch: {
      label:      'Format mismatch',
      pill:       'Mismatch',
      cls:        'pb-status-mismatch',
      color:      'var(--status-mismatch)',
      resolution: 'Update format on Encora',
      actionable: true,
    },
    missing_file: {
      label:      'Missing file',
      pill:       'Missing',
      cls:        'pb-status-missing',
      color:      'var(--status-missing)',
      resolution: 'Download from trader',
      actionable: false,
    },
    wanted_file: {
      label:      'Wanted file',
      pill:       'Wanted',
      cls:        'pb-status-wanted',
      color:      'var(--status-wanted)',
      resolution: 'Download from trader',
      actionable: false,
    },
  };

  // TYPE_FILTERS lists the filter tabs in display order. The empty
  // key is the All tab.
  var TYPE_FILTERS = [
    { key: '',                  label: 'All' },
    { key: 'add_to_collection', label: 'Add to collection' },
    { key: 'format_mismatch',   label: 'Format mismatch' },
    { key: 'missing_file',      label: 'Missing file' },
    { key: 'wanted_file',       label: 'Wanted file' },
  ];

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function getActiveType() {
    var p = new URLSearchParams(window.location.search);
    var t = (p.get('type') || '').toLowerCase();
    var canonical = TYPE_FILTERS.find(function (f) {
      return f.key.toLowerCase() === t;
    });
    return canonical ? canonical.key : '';
  }

  function setActiveType(key) {
    var url = new URL(window.location.href);
    if (key) url.searchParams.set('type', key);
    else     url.searchParams.delete('type');
    window.history.pushState({}, '', url.toString());
    render();
  }

  function countByType(items) {
    var out = { '': items.length };
    TYPE_FILTERS.forEach(function (f) { if (f.key) out[f.key] = 0; });
    items.forEach(function (it) {
      if (out[it.type] != null) out[it.type]++;
    });
    return out;
  }

  // selected holds recording ids whose actionable rows the user has
  // ticked. Lookups here drive both the row tint and the Push button's
  // enabled state.
  var selected = new Set();

  // cachedItems holds the last successful fetch so tab clicks re-
  // render synchronously without re-hitting the network.
  var cachedItems = null;

  // itemsById maps recording id → item for quick lookups when building
  // the apply payload.
  var itemsById = {};

  function rebuildIndex(items) {
    itemsById = {};
    items.forEach(function (it) { itemsById[it.recording_id] = it; });
  }

  function actionableSelectedCount() {
    var n = 0;
    selected.forEach(function (id) {
      var it = itemsById[id];
      if (it && TYPE_META[it.type] && TYPE_META[it.type].actionable) n++;
    });
    return n;
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (sub) {
      sub.textContent = 'Differences between your library and Encora · ' +
        items.length + ' pending';
    }

    var actions = document.querySelector('[data-page-actions]');
    if (!actions) return;
    var n = actionableSelectedCount();
    var disabled = n === 0 ? ' disabled' : '';
    actions.innerHTML =
      '<span class="pb-mono pb-muted" data-mm-count>' + n + ' selected</span>' +
      '<button class="pb-btn pb-btn-ghost" type="button" data-mm-skip>' +
        'Skip selected' +
      '</button>' +
      '<button class="pb-btn pb-btn-primary" type="button" data-mm-push' +
        disabled + '>Push ' + n + ' to Encora</button>';

    var skipBtn = actions.querySelector('[data-mm-skip]');
    if (skipBtn) {
      skipBtn.addEventListener('click', function () {
        if (selected.size === 0) return;
        selected.clear();
        render();
      });
    }
    var pushBtn = actions.querySelector('[data-mm-push]');
    if (pushBtn) {
      pushBtn.addEventListener('click', onPushClick);
    }
  }

  function renderCallout(root) {
    var html = '<div class="pb-callout" style="' +
      'background: var(--bg-card); border-color: var(--rule); color: var(--ink-2);">' +
      '<div>' +
        '<strong>Reconciling local disk with Encora</strong>' +
        'Promptbook is the source of truth for what’s on disk; Encora is ' +
        'the source of truth for what recordings exist. These items differ — ' +
        'pick which to push. ' +
        '<a class="pb-linkmono" href="/sync">Read more →</a>' +
      '</div>' +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderTabs(root, items, active) {
    var counts = countByType(items);
    var html = '<div class="pb-tabs" role="tablist">';
    TYPE_FILTERS.forEach(function (f) {
      var meta = f.key ? TYPE_META[f.key] : null;
      var current = (f.key === active) ? 'true' : 'false';
      var dotStyle = meta ? ' style="--dot:' + meta.color + '"' : '';
      var dotEl = meta ? '<span class="pb-tab-dot"></span>' : '';
      html += '<button class="pb-tab" role="tab" data-type="' + escapeHTML(f.key) +
              '" aria-current="' + current + '"' + dotStyle + '>' +
              dotEl + '<span>' + escapeHTML(f.label) + '</span>' +
              '<span class="pb-tab-count">' + (counts[f.key] || 0) + '</span>' +
              '</button>';
    });
    html += '</div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('.pb-tab').forEach(function (btn) {
      btn.addEventListener('click', function () {
        setActiveType(btn.getAttribute('data-type') || '');
      });
    });
  }

  function renderTable(root, items, active) {
    var filtered = active ?
      items.filter(function (it) { return it.type === active; }) :
      items.slice();

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th style="width:32px"></th>' +
        '<th>Type</th>' +
        '<th>Recording</th>' +
        '<th>Detail</th>' +
        '<th style="text-align:right">Resolution</th>' +
      '</tr></thead><tbody>';

    if (filtered.length === 0) {
      html += '<tr><td colspan="5" class="pb-empty" style="padding:32px 14px">' +
              (items.length === 0 ?
                'Everything synced. Encora and your library agree.' :
                'No mismatches match this filter.') +
              '</td></tr>';
    }

    filtered.forEach(function (it) {
      var meta = TYPE_META[it.type] || TYPE_META.add_to_collection;
      var subtitle = (it.tour ? escapeHTML(it.tour) + ' · ' : '') +
                     'enc-' + it.recording_id;
      var isSelected = selected.has(it.recording_id);
      var rowStyle = isSelected ?
        ' style="background: color-mix(in oklab, var(--accent) 4%, transparent)"' : '';
      var checkbox;
      if (meta.actionable) {
        checkbox = '<input type="checkbox" class="pb-mm-cb" ' +
          'data-id="' + it.recording_id + '"' +
          (isSelected ? ' checked' : '') + '>';
      } else {
        checkbox = '<input type="checkbox" class="pb-mm-cb" disabled ' +
          'title="Acquire the file separately to resolve this mismatch">';
      }
      var detail = '<span class="pb-mono">' +
                     escapeHTML(it.description || '') +
                   '</span>';
      if (!meta.actionable) {
        detail += ' <span class="pb-muted pb-mono" style="font-size:11px">' +
                  '(needs download)</span>';
      }
      html += '<tr data-id="' + it.recording_id + '"' + rowStyle + '>' +
        '<td>' + checkbox + '</td>' +
        '<td><span class="pb-badge ' + meta.cls + '">' +
          '<span class="pb-badge-dot"></span>' +
          escapeHTML(meta.pill) +
        '</span></td>' +
        '<td class="pb-cell-show">' + escapeHTML(it.show || '—') +
          '<small>' + subtitle + '</small></td>' +
        '<td>' + detail + '</td>' +
        '<td class="pb-cell-mono" style="text-align:right; color: var(--accent)">' +
          '→ ' + escapeHTML(meta.resolution) +
        '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);

    root.querySelectorAll('input.pb-mm-cb[data-id]').forEach(function (cb) {
      cb.addEventListener('change', function (ev) {
        var id = parseInt(cb.getAttribute('data-id'), 10);
        if (!isFinite(id)) return;
        if (cb.checked) selected.add(id);
        else            selected.delete(id);
        // Avoid a full re-render on every click; just patch the row
        // tint and the header counters.
        var row = cb.closest('tr');
        if (row) {
          row.setAttribute('style', cb.checked ?
            'background: color-mix(in oklab, var(--accent) 4%, transparent)' :
            '');
        }
        renderHeader(cachedItems || []);
        ev.stopPropagation();
      });
    });
  }

  function onPushClick() {
    var actions = [];
    selected.forEach(function (id) {
      var it = itemsById[id];
      if (!it) return;
      var meta = TYPE_META[it.type];
      if (!meta || !meta.actionable) return;
      var act = {
        type:         it.type,
        recording_id: it.recording_id,
        new_format:   '',
      };
      if (it.type === 'format_mismatch') {
        act.new_format = it.local_format || '';
      }
      actions.push(act);
    });
    if (actions.length === 0) return;

    var msg = 'Push ' + actions.length + ' change' +
      (actions.length === 1 ? '' : 's') +
      ' to Encora? This is irreversible.';
    if (!window.confirm(msg)) return;

    var pushBtn = document.querySelector('[data-mm-push]');
    if (pushBtn) pushBtn.setAttribute('disabled', 'disabled');

    window.PB.api.post('/apply', { actions: actions })
      .then(function (body) {
        renderApplyResults((body && body.results) || []);
      })
      .catch(function (err) {
        renderApplyError(err);
      });
  }

  function renderApplyResults(results) {
    var root = document.getElementById('page-root');
    if (!root) return;
    var ok = 0;
    results.forEach(function (r) { if (r.ok) ok++; });

    var sub = document.querySelector('[data-page-sub]');
    if (sub) {
      sub.textContent = ok + ' of ' + results.length + ' action(s) succeeded';
    }
    var actions = document.querySelector('[data-page-actions]');
    if (actions) {
      actions.innerHTML =
        '<a class="pb-btn" href="#" data-mm-reload>Reload</a>' +
        '<a class="pb-btn" href="/history">View history</a>';
      var reload = actions.querySelector('[data-mm-reload]');
      if (reload) {
        reload.addEventListener('click', function (ev) {
          ev.preventDefault();
          selected.clear();
          init();
        });
      }
    }

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th>Status</th>' +
        '<th>Type</th>' +
        '<th>Recording</th>' +
        '<th>Detail</th>' +
      '</tr></thead><tbody>';

    if (results.length === 0) {
      html += '<tr><td colspan="4" class="pb-empty" style="padding:32px 14px">' +
              'No actions submitted.</td></tr>';
    }

    results.forEach(function (r) {
      var act = r.action || {};
      var meta = TYPE_META[act.type] || {};
      var pillClass = r.ok ? 'pb-apply-ok' : 'pb-apply-err';
      var pillLabel = r.ok ? 'OK' : 'Error';
      var detail = r.ok ?
        (act.new_format ?
          '<span class="pb-mono">format → ' +
            escapeHTML(act.new_format) + '</span>' :
          '<span class="pb-muted">pushed</span>') :
        '<span class="pb-muted">' + escapeHTML(r.error || '') + '</span>';
      html += '<tr>' +
        '<td><span class="pb-badge-square ' + pillClass + '">' + pillLabel + '</span></td>' +
        '<td><span class="pb-badge-square ' + escapeHTML(meta.cls || 'pb-status-orphan') + '">' +
          escapeHTML(act.type || '') +
        '</span></td>' +
        '<td><a class="pb-link" href="/recordings/' +
          escapeHTML(String(act.recording_id || '')) + '">' +
          escapeHTML(String(act.recording_id || '')) +
        '</a></td>' +
        '<td>' + detail + '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';

    // Replace everything below the page header (callout, tabs, table)
    // with the result table so the user sees outcome + reload affordance.
    root.innerHTML = '';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderApplyError(err) {
    var root = document.getElementById('page-root');
    if (!root) return;
    root.innerHTML = '<div class="pb-empty">Apply failed: ' +
      escapeHTML(err && err.message ? err.message : String(err)) +
      '</div>';
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load mismatches: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function render() {
    var root = document.getElementById('page-root');
    if (!root) return;
    if (!cachedItems) return;
    var active = getActiveType();
    root.innerHTML = '';
    renderHeader(cachedItems);
    if (cachedItems.length === 0) {
      root.insertAdjacentHTML('beforeend',
        '<div class="pb-empty">' +
        'Everything synced. Encora and your library agree.' +
        '</div>');
      return;
    }
    renderCallout(root);
    renderTabs(root, cachedItems, active);
    renderTable(root, cachedItems, active);
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="mismatches"]');
    if (!root) return;
    cachedItems = null;
    window.PB.api.get('/mismatches')
      .then(function (body) {
        cachedItems = (body && body.items) || [];
        rebuildIndex(cachedItems);
        // Drop selections that no longer match any item — guards
        // against a reload after partial apply.
        var stale = [];
        selected.forEach(function (id) {
          if (!itemsById[id]) stale.push(id);
        });
        stale.forEach(function (id) { selected.delete(id); });
        render();
      })
      .catch(function (err) {
        renderError(root, err);
      });
  }

  // Bind popstate once so back/forward through tab clicks re-renders
  // the cached data without re-fetching.
  window.addEventListener('popstate', render);

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
