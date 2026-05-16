// history.js — fetches /api/v1/history and renders the activity log.
//
// Kind filter tabs come from the design's "All + 6 kinds" spec. Counts
// are computed client-side from a single unfiltered fetch — far cheaper
// than 6 fetches and the dataset is small (50-page paginated).
//
// Each row is expandable to show its `details` JSON pretty-printed in
// a `.pb-json` block. Expansion state lives in client memory only — no
// URL or server round-trip — so a refresh resets the view.

(function () {
  'use strict';

  var KIND_META = {
    ingest:         { label: 'Ingest',        cls: 'pb-status-synced',   color: 'var(--status-synced)' },
    rename:         { label: 'Rename',        cls: 'pb-status-mismatch', color: 'var(--status-mismatch)' },
    nfo_write:      { label: 'NFO Write',     cls: 'pb-status-wanted',   color: 'var(--status-wanted)' },
    encora_push:    { label: 'Encora Push',   cls: 'pb-status-missing',  color: 'var(--accent)' },
    sync:           { label: 'Sync',          cls: 'pb-status-orphan',   color: 'var(--status-orphan)' },
    manual_import:  { label: 'Manual Import', cls: 'pb-status-missing',  color: 'var(--status-missing)' },
  };

  var KIND_FILTERS = [
    { key: '',              label: 'All' },
    { key: 'ingest',        label: 'Ingest' },
    { key: 'rename',        label: 'Rename' },
    { key: 'nfo_write',     label: 'NFO Write' },
    { key: 'encora_push',   label: 'Encora Push' },
    { key: 'sync',          label: 'Sync' },
    { key: 'manual_import', label: 'Manual Import' },
  ];

  var PAGE_SIZE = 50;

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function formatTime(iso) {
    if (!iso) return '—';
    var t = new Date(iso);
    if (isNaN(t.getTime())) return '—';
    function pad(n) { return n < 10 ? '0' + n : '' + n; }
    return pad(t.getHours()) + ':' + pad(t.getMinutes()) + ':' + pad(t.getSeconds());
  }

  function todayCount(items) {
    var todayPrefix = new Date().toISOString().substring(0, 10);
    return items.filter(function (it) {
      return (it.occurred_at || '').substring(0, 10) === todayPrefix;
    }).length;
  }

  // prettyJSON renders a value as a multi-line string with span wrappers
  // for keys/strings/numbers. Stays within the .pb-json palette.
  function prettyJSON(value, indent) {
    indent = indent || 0;
    var pad = '  '.repeat(indent);
    if (value === null) return '<span class="n">null</span>';
    if (typeof value === 'boolean') return '<span class="n">' + value + '</span>';
    if (typeof value === 'number') return '<span class="n">' + value + '</span>';
    if (typeof value === 'string') {
      return '<span class="s">' + escapeHTML(JSON.stringify(value)) + '</span>';
    }
    if (Array.isArray(value)) {
      if (value.length === 0) return '[]';
      var inner = value.map(function (v) {
        return '  '.repeat(indent + 1) + prettyJSON(v, indent + 1);
      }).join(',\n');
      return '[\n' + inner + '\n' + pad + ']';
    }
    if (typeof value === 'object') {
      var keys = Object.keys(value);
      if (keys.length === 0) return '{}';
      var inner2 = keys.map(function (k) {
        return '  '.repeat(indent + 1) +
          '<span class="k">' + escapeHTML(JSON.stringify(k)) + '</span>: ' +
          prettyJSON(value[k], indent + 1);
      }).join(',\n');
      return '{\n' + inner2 + '\n' + pad + '}';
    }
    return escapeHTML(String(value));
  }

  function getActiveKind() {
    var p = new URLSearchParams(window.location.search);
    var k = (p.get('kind') || '').toLowerCase();
    var canonical = KIND_FILTERS.find(function (f) { return f.key === k; });
    return canonical ? canonical.key : '';
  }

  function setActiveKind(key) {
    var url = new URL(window.location.href);
    if (key) url.searchParams.set('kind', key);
    else     url.searchParams.delete('kind');
    window.history.pushState({}, '', url.toString());
    render();
  }

  function countByKind(items) {
    var out = { '': items.length };
    KIND_FILTERS.forEach(function (f) { if (f.key) out[f.key] = 0; });
    items.forEach(function (it) {
      if (out[it.kind] != null) out[it.kind]++;
    });
    return out;
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (sub) {
      var today = new Date();
      var dateLabel = today.toLocaleDateString(undefined,
        { year: 'numeric', month: 'long', day: 'numeric' });
      sub.textContent = dateLabel + ' · ' +
        todayCount(items) + ' events today · ' +
        items.length + ' total · retention configurable';
    }
    var actions = document.querySelector('[data-page-actions]');
    if (!actions || actions.dataset.rendered) return;
    actions.dataset.rendered = '1';
    actions.innerHTML =
      '<button class="pb-btn pb-btn-ghost" type="button" title="(coming soon)">Date range</button>' +
      '<a class="pb-btn pb-btn-ghost" href="/api/v1/history?limit=10000" download="promptbook-history.json">' +
        'Export JSON</a>';
  }

  function renderTabs(root, items, active) {
    var counts = countByKind(items);
    var html = '<div class="pb-tabs" role="tablist">';
    KIND_FILTERS.forEach(function (f) {
      var meta = f.key ? KIND_META[f.key] : null;
      var current = (f.key === active) ? 'true' : 'false';
      var dotStyle = meta ? ' style="--dot:' + meta.color + '"' : '';
      var dotEl = meta ? '<span class="pb-tab-dot"></span>' : '';
      html += '<button class="pb-tab" role="tab" data-kind="' + escapeHTML(f.key) +
              '" aria-current="' + current + '"' + dotStyle + '>' +
              dotEl + '<span>' + escapeHTML(f.label) + '</span>' +
              '<span class="pb-tab-count">' + (counts[f.key] || 0) + '</span>' +
              '</button>';
    });
    html += '</div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('.pb-tab').forEach(function (btn) {
      btn.addEventListener('click', function () {
        setActiveKind(btn.getAttribute('data-kind') || '');
      });
    });
  }

  function renderTable(root, items, active, page) {
    var filtered = active ?
      items.filter(function (it) { return it.kind === active; }) :
      items.slice();
    var visible = filtered.slice(0, page * PAGE_SIZE);

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th style="width:32px"></th>' +
        '<th style="width:90px">Time</th>' +
        '<th style="width:140px">Kind</th>' +
        '<th style="width:140px">Recording</th>' +
        '<th>Summary</th>' +
      '</tr></thead><tbody>';
    if (visible.length === 0) {
      html += '<tr><td colspan="5" class="pb-empty" style="padding:32px 14px;text-align:center">' +
        (items.length === 0 ?
          'No events yet. Run <code>promptbook collection sync</code> or ingest some recordings to populate the log.' :
          'No events match this filter.') +
        '</td></tr>';
    }
    visible.forEach(function (it) {
      var meta = KIND_META[it.kind] || { label: it.kind, cls: 'pb-status-orphan' };
      var recCell = it.recording_id ?
        '<a class="pb-cell-mono pb-link" href="/recordings/' + it.recording_id + '">' +
          'enc-' + it.recording_id + '</a>' :
        '<span class="pb-muted">—</span>';
      html += '<tr class="pb-history-row" data-event-id="' + it.id + '">' +
        '<td><span class="pb-history-chev" aria-hidden="true">▸</span></td>' +
        '<td class="pb-cell-mono">' + escapeHTML(formatTime(it.occurred_at)) + '</td>' +
        '<td><span class="pb-badge-square ' + meta.cls + '">' +
          escapeHTML(meta.label) + '</span></td>' +
        '<td>' + recCell + '</td>' +
        '<td>' + escapeHTML(it.summary || '') + '</td>' +
      '</tr>';
      var detailsObj = it.details && Object.keys(it.details).length ? it.details : null;
      if (detailsObj) {
        html += '<tr class="pb-history-detail" data-detail-for="' + it.id + '" hidden>' +
          '<td></td>' +
          '<td colspan="4"><div class="pb-json">' + prettyJSON(detailsObj) + '</div></td>' +
        '</tr>';
      }
    });
    html += '</tbody></table></div>';

    if (filtered.length > visible.length) {
      html += '<div style="margin-top:14px;text-align:center">' +
        '<button class="pb-btn" type="button" data-load-more>' +
          'Load ' + Math.min(PAGE_SIZE, filtered.length - visible.length) + ' more' +
        '</button></div>';
    }

    root.insertAdjacentHTML('beforeend', html);

    root.querySelectorAll('tr.pb-history-row').forEach(function (tr) {
      tr.addEventListener('click', function () {
        var id = tr.getAttribute('data-event-id');
        var detail = root.querySelector('tr.pb-history-detail[data-detail-for="' + id + '"]');
        if (!detail) return;
        var nowHidden = detail.hasAttribute('hidden');
        if (nowHidden) detail.removeAttribute('hidden');
        else           detail.setAttribute('hidden', '');
        var chev = tr.querySelector('.pb-history-chev');
        if (chev) chev.textContent = nowHidden ? '▾' : '▸';
      });
    });

    var more = root.querySelector('[data-load-more]');
    if (more) {
      more.addEventListener('click', function () {
        currentPage++;
        render();
      });
    }
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty" style="padding:32px;color:var(--status-missing)">' +
      'Failed to load history: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  var cachedItems = null;
  var currentPage = 1;

  function render() {
    var root = document.getElementById('page-root');
    if (!root || !cachedItems) return;
    var active = getActiveKind();
    root.innerHTML = '';
    renderHeader(cachedItems);
    renderTabs(root, cachedItems, active);
    renderTable(root, cachedItems, active, currentPage);
  }

  function init() {
    var root = document.getElementById('page-root');
    if (!root || root.getAttribute('data-page') !== 'history') return;
    window.PB.api.get('/history?limit=500')
      .then(function (body) {
        cachedItems = (body && body.items) || [];
        render();
      })
      .catch(function (err) { renderError(root, err); });
    window.addEventListener('popstate', render);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
