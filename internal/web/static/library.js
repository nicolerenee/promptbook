// library.js — fetches /api/v1/recordings and renders the Library page.
//
// Architectural note: this is the reference implementation for the
// API-only page pattern. The server template ships an empty
// <div id="page-root" data-page="library"></div> shell; this script
// fetches data from the JSON API and populates the DOM. Subsequent
// page agents should mirror this shape.
//
// NFT callout copy (for the recording detail agent — not used here):
//   Library doesn't render the callout, but the corrected copy is:
//     - With date:    "Not For Trade — until YYYY-MM-DD."
//                     "Do not share or trade this recording until the date passes."
//     - Forever:      "Not For Trade — permanent."
//                     "Do not share or trade this recording."
//   "NFT" stands for "Not For Trade", not the design's "no-further-trade".

(function () {
  'use strict';

  var STATUS_META = {
    Synced:         { label: 'Synced',          cls: 'pb-status-synced',   color: 'var(--status-synced)' },
    FormatMismatch: { label: 'Format mismatch', cls: 'pb-status-mismatch', color: 'var(--status-mismatch)' },
    Missing:        { label: 'Missing',         cls: 'pb-status-missing',  color: 'var(--status-missing)' },
    Wanted:         { label: 'Wanted',          cls: 'pb-status-wanted',   color: 'var(--status-wanted)' },
    Orphan:         { label: 'Orphan',          cls: 'pb-status-orphan',   color: 'var(--status-orphan)' },
  };

  // STATUS_FILTERS lists the filter tabs in display order. The empty key
  // is the All tab.
  var STATUS_FILTERS = [
    { key: '',               label: 'All' },
    { key: 'Synced',         label: 'Synced' },
    { key: 'FormatMismatch', label: 'Format mismatch' },
    { key: 'Missing',        label: 'Missing' },
    { key: 'Wanted',         label: 'Wanted' },
    { key: 'Orphan',         label: 'Orphan' },
  ];

  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
                'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  // smartDate mirrors the server-side smartDate template helper so the
  // table reads consistently with the rename engine's {Date} token.
  function smartDate(full, monthKnown, dayKnown) {
    if (!full) return '—';
    if (!monthKnown) return full.substring(0, 4);
    if (!dayKnown) {
      var mm = parseInt(full.substring(5, 7), 10);
      if (!isFinite(mm) || mm < 1 || mm > 12) return full;
      return MONTHS[mm - 1] + ' ' + full.substring(0, 4);
    }
    return full;
  }

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function getActiveStatus() {
    var p = new URLSearchParams(window.location.search);
    var s = p.get('status') || '';
    // Tolerate the legacy lowercase tokens the old server used.
    var canonical = STATUS_FILTERS.find(function (f) {
      return f.key.toLowerCase() === s.toLowerCase();
    });
    return canonical ? canonical.key : '';
  }

  function setActiveStatus(key) {
    var url = new URL(window.location.href);
    if (key) url.searchParams.set('status', key);
    else     url.searchParams.delete('status');
    window.history.pushState({}, '', url.toString());
    render();
  }

  function countByStatus(items) {
    var out = { '': items.length };
    STATUS_FILTERS.forEach(function (f) { if (f.key) out[f.key] = 0; });
    items.forEach(function (it) {
      if (out[it.status] != null) out[it.status]++;
    });
    return out;
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (!sub) return;
    var counts = countByStatus(items);
    var synced = counts['Synced'] || 0;
    var wanted = counts['Wanted'] || 0;
    var fileCount = items.reduce(function (n, it) { return n + (it.file_count || 0); }, 0);
    sub.textContent = items.length + ' cataloged · ' +
                      synced + ' synced · ' +
                      wanted + ' wanted · ' +
                      fileCount + ' files on disk';

    var actions = document.querySelector('[data-page-actions]');
    if (actions && !actions.dataset.rendered) {
      actions.dataset.rendered = '1';
      actions.innerHTML =
        '<button class="pb-btn pb-btn-ghost" type="button">Filters</button>' +
        '<button class="pb-btn" type="button">Sync now</button>' +
        '<button class="pb-btn pb-btn-primary" type="button">Manual import</button>';
    }
  }

  function renderMetrics(root, items) {
    var counts = countByStatus(items);
    var fileCount = items.reduce(function (n, it) { return n + (it.file_count || 0); }, 0);
    var html =
      '<div class="pb-metrics">' +
        metricTile('Synced',     counts['Synced'] || 0,         items.length + ' cataloged') +
        metricTile('Wanted',     counts['Wanted'] || 0,         'on the shopping list') +
        metricTile('Mismatches', (counts['FormatMismatch'] || 0) +
                                 (counts['Missing'] || 0) +
                                 (counts['Orphan'] || 0),        'needs reconcile') +
        metricTile('Files on disk', fileCount,                  'across all versions') +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function metricTile(label, num, sub) {
    return '<div class="pb-metric">' +
      '<div class="pb-metric-label">' + escapeHTML(label) + '</div>' +
      '<div class="pb-metric-num">' + escapeHTML(String(num)) + '</div>' +
      '<div class="pb-metric-sub">' + escapeHTML(sub) + '</div>' +
    '</div>';
  }

  function renderTabs(root, items, active) {
    var counts = countByStatus(items);
    var html = '<div class="pb-tabs" role="tablist">';
    STATUS_FILTERS.forEach(function (f) {
      var meta = f.key ? STATUS_META[f.key] : null;
      var current = (f.key === active) ? 'true' : 'false';
      var dotStyle = meta ? ' style="--dot:' + meta.color + '"' : '';
      var dotEl = meta ? '<span class="pb-tab-dot"></span>' : '';
      html += '<button class="pb-tab" role="tab" data-status="' + escapeHTML(f.key) +
              '" aria-current="' + current + '"' + dotStyle + '>' +
              dotEl + '<span>' + escapeHTML(f.label) + '</span>' +
              '<span class="pb-tab-count">' + (counts[f.key] || 0) + '</span>' +
              '</button>';
    });
    html += '</div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('.pb-tab').forEach(function (btn) {
      btn.addEventListener('click', function () {
        setActiveStatus(btn.getAttribute('data-status') || '');
      });
    });
  }

  function renderTable(root, items, active) {
    var filtered = active ?
      items.filter(function (it) { return it.status === active; }) :
      items.slice();

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th>Status</th>' +
        '<th>Recording</th>' +
        '<th>Date</th>' +
        '<th>Master</th>' +
        '<th>Local format</th>' +
        '<th>Encora format</th>' +
      '</tr></thead><tbody>';
    if (filtered.length === 0) {
      html += '<tr><td colspan="6" class="pb-empty" style="padding:32px 14px">' +
              'No recordings match this filter.</td></tr>';
    }
    filtered.forEach(function (it) {
      var meta = STATUS_META[it.status] || STATUS_META.Orphan;
      var subtitle = (it.tour ? escapeHTML(it.tour) + ' · ' : '') + 'enc-' + it.id;
      var nft = it.nft ? ' <span title="Under NFT" style="color:var(--nft-rule)">⛔</span>' : '';
      html += '<tr class="pb-row-link" data-href="/recordings/' + it.id + '">' +
        '<td class="pb-tr-status" style="--row-c:' + meta.color + '">' +
          '<span class="pb-badge-square ' + meta.cls + '">' + escapeHTML(meta.label) + '</span>' +
        '</td>' +
        '<td class="pb-cell-show">' + escapeHTML(it.show || '—') + nft +
          '<small>' + subtitle + '</small></td>' +
        '<td class="pb-cell-mono">' +
          escapeHTML(smartDate(it.date_full, it.date_month_known, it.date_day_known)) +
        '</td>' +
        '<td>' + escapeHTML(it.master || '—') + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(it.local_format || '—') + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(it.encora_format || '—') + '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('tr.pb-row-link').forEach(function (tr) {
      tr.addEventListener('click', function () {
        window.location.href = tr.getAttribute('data-href');
      });
    });
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load recordings: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  // cachedItems holds the last successfully fetched recordings list so a
  // tab click can re-render synchronously without hitting the network.
  var cachedItems = null;

  function render() {
    var root = document.getElementById('page-root');
    if (!root) return;
    if (!cachedItems) return;
    var active = getActiveStatus();
    root.innerHTML = '';
    renderHeader(cachedItems);
    renderMetrics(root, cachedItems);
    renderTabs(root, cachedItems, active);
    renderTable(root, cachedItems, active);
  }

  function init() {
    var root = document.getElementById('page-root');
    if (!root || root.getAttribute('data-page') !== 'library') return;
    window.PB.api.get('/recordings?limit=200')
      .then(function (body) {
        cachedItems = (body && body.items) || [];
        render();
      })
      .catch(function (err) {
        renderError(root, err);
      });
    window.addEventListener('popstate', render);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
