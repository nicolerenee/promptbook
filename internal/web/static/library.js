// library.js — fetches /api/v1/recordings and renders the Library page.
//
// Architectural note: this is the reference implementation for the
// API-only page pattern. The server template ships an empty
// <div id="page-root" data-page="library"></div> shell; this script
// fetches data from the JSON API and populates the DOM. Subsequent
// page agents should mirror this shape.
//
// Status string casing: STATUS_META and STATUS_FILTERS key on the
// lowercase API tokens ("synced", "format_mismatch", "missing",
// "wanted", "orphan") so STATUS_META[item.status] resolves directly
// against /api/v1/recordings JSON. The user-visible labels stay
// title-cased.
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
    synced:          { label: 'Synced',          cls: 'pb-status-synced',   color: 'var(--status-synced)' },
    format_mismatch: { label: 'Format mismatch', cls: 'pb-status-mismatch', color: 'var(--status-mismatch)' },
    missing:         { label: 'Missing',         cls: 'pb-status-missing',  color: 'var(--status-missing)' },
    wanted:          { label: 'Wanted',          cls: 'pb-status-wanted',   color: 'var(--status-wanted)' },
    orphan:          { label: 'Orphan',          cls: 'pb-status-orphan',   color: 'var(--status-orphan)' },
  };

  // STATUS_FILTERS lists the filter tabs in display order. The empty key
  // is the All tab.
  var STATUS_FILTERS = [
    { key: '',                label: 'All' },
    { key: 'synced',          label: 'Synced' },
    { key: 'format_mismatch', label: 'Format mismatch' },
    { key: 'missing',         label: 'Missing' },
    { key: 'wanted',          label: 'Wanted' },
    { key: 'orphan',          label: 'Orphan' },
  ];

  // SORT_COLUMNS lists every sortable column in render order. `key` is
  // the URL token persisted in ?sort=<key>; `label` matches the table
  // header text; `compare` is a stable comparator on two list items.
  // localFormat sorts empty/null last on ascending so unmatched files
  // group at the bottom rather than the top.
  var SORT_COLUMNS = [
    {
      key: 'status',
      label: 'Status',
      compare: function (a, b) { return cmpStr(a.status, b.status); },
    },
    {
      key: 'recording',
      label: 'Recording',
      compare: function (a, b) {
        var c = cmpStr(a.show, b.show);
        if (c !== 0) return c;
        c = cmpStr(a.tour, b.tour);
        if (c !== 0) return c;
        return cmpNum(a.id, b.id);
      },
    },
    {
      key: 'date',
      label: 'Date',
      compare: function (a, b) { return cmpStr(a.date_full, b.date_full); },
    },
    {
      key: 'master',
      label: 'Master',
      compare: function (a, b) { return cmpStr(a.master, b.master); },
    },
    {
      key: 'local_format',
      label: 'Local format',
      compare: function (a, b) { return cmpEmptyLast(a.local_format, b.local_format); },
    },
  ];

  var DEFAULT_SORT = { key: 'date', dir: 'desc' };

  // cmpStr is a case-insensitive lexicographic comparator. null/undefined
  // sort before any real string so the result is stable.
  function cmpStr(a, b) {
    var sa = (a == null) ? '' : String(a).toLowerCase();
    var sb = (b == null) ? '' : String(b).toLowerCase();
    if (sa < sb) return -1;
    if (sa > sb) return 1;
    return 0;
  }

  // cmpNum compares numerics; non-numeric inputs collapse to 0.
  function cmpNum(a, b) {
    var na = Number(a) || 0;
    var nb = Number(b) || 0;
    return na - nb;
  }

  // cmpEmptyLast is cmpStr with the twist that empty/null values sort
  // after any non-empty value regardless of direction. Used so unmatched
  // local_format rows fall to the bottom on both asc and desc.
  function cmpEmptyLast(a, b) {
    var ea = (a == null || a === '');
    var eb = (b == null || b === '');
    if (ea && !eb) return 1;
    if (!ea && eb) return -1;
    if (ea && eb) return 0;
    return cmpStr(a, b);
  }

  // smartDate renders an ISO date with the precision the catalog
  // recorded:
  //   full date known           → YYYY-MM-DD
  //   day unknown, month known  → YYYY-MM
  //   month unknown             → YYYY
  //   no date at all            → —
  // The dash placeholder matches the UI convention.
  function smartDate(full, monthKnown, dayKnown) {
    if (!full) return '—';
    if (!monthKnown) return full.substring(0, 4);
    if (!dayKnown) return full.substring(0, 7);
    return full.substring(0, 10);
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

  // getActiveSort reads the URL's ?sort and ?dir parameters and falls
  // back to DEFAULT_SORT (date desc) when either is missing or invalid.
  function getActiveSort() {
    var p = new URLSearchParams(window.location.search);
    var key = p.get('sort') || '';
    var dir = (p.get('dir') || '').toLowerCase();
    var col = SORT_COLUMNS.find(function (c) { return c.key === key; });
    if (!col) return { key: DEFAULT_SORT.key, dir: DEFAULT_SORT.dir };
    if (dir !== 'asc' && dir !== 'desc') dir = 'asc';
    return { key: col.key, dir: dir };
  }

  // setActiveSort updates the URL's ?sort/?dir to reflect the user's
  // click. When the column already matches the active sort, clicking
  // toggles between asc/desc; otherwise the column gets the default
  // direction (desc for date, asc for everything else).
  function setActiveSort(key) {
    var current = getActiveSort();
    var dir;
    if (current.key === key) {
      dir = (current.dir === 'asc') ? 'desc' : 'asc';
    } else {
      dir = (key === 'date') ? 'desc' : 'asc';
    }
    var url = new URL(window.location.href);
    url.searchParams.set('sort', key);
    url.searchParams.set('dir', dir);
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
    var synced = counts['synced'] || 0;
    var wanted = counts['wanted'] || 0;
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
        metricTile('Synced',     counts['synced'] || 0,         items.length + ' cataloged') +
        metricTile('Wanted',     counts['wanted'] || 0,         'on the shopping list') +
        metricTile('Mismatches', (counts['format_mismatch'] || 0) +
                                 (counts['missing'] || 0) +
                                 (counts['orphan'] || 0),       'needs reconcile') +
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

  // sortItems returns a new array sorted by the active column + dir.
  // Doesn't mutate the input so cachedItems retains insertion order
  // for unrelated re-renders (e.g. status filter clicks).
  function sortItems(items, sort) {
    var col = SORT_COLUMNS.find(function (c) { return c.key === sort.key; });
    if (!col) return items.slice();
    var sign = (sort.dir === 'desc') ? -1 : 1;
    var out = items.slice();
    out.sort(function (a, b) {
      // cmpEmptyLast keeps empty values pinned to the bottom regardless
      // of direction, so we don't apply `sign` to its result.
      if (col.key === 'local_format') return col.compare(a, b);
      return sign * col.compare(a, b);
    });
    return out;
  }

  function renderTable(root, items, active, sort) {
    var filtered = active ?
      items.filter(function (it) { return it.status === active; }) :
      items.slice();
    filtered = sortItems(filtered, sort);

    var headerCells = SORT_COLUMNS.map(function (col) {
      var isActive = (col.key === sort.key);
      var caret = '';
      if (isActive) caret = (sort.dir === 'desc') ? ' ▼' : ' ▲';
      var ariaSort = isActive
        ? (sort.dir === 'desc' ? 'descending' : 'ascending')
        : 'none';
      return '<th class="pb-th-sort" data-sort-key="' + escapeHTML(col.key) +
        '" aria-sort="' + ariaSort + '" tabindex="0" role="button"' +
        (isActive ? ' data-sort-active="1"' : '') + '>' +
        escapeHTML(col.label) + escapeHTML(caret) +
        '</th>';
    }).join('');

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' + headerCells + '</tr></thead><tbody>';
    if (filtered.length === 0) {
      html += '<tr><td colspan="' + SORT_COLUMNS.length + '" class="pb-empty" ' +
              'style="padding:32px 14px">No recordings match this filter.</td></tr>';
    }
    filtered.forEach(function (it) {
      var meta = STATUS_META[it.status] || STATUS_META.orphan;
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
      '</tr>';
    });
    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('tr.pb-row-link').forEach(function (tr) {
      tr.addEventListener('click', function () {
        window.location.href = tr.getAttribute('data-href');
      });
    });
    root.querySelectorAll('th.pb-th-sort').forEach(function (th) {
      th.addEventListener('click', function () {
        setActiveSort(th.getAttribute('data-sort-key'));
      });
      th.addEventListener('keydown', function (ev) {
        if (ev.key === 'Enter' || ev.key === ' ') {
          ev.preventDefault();
          setActiveSort(th.getAttribute('data-sort-key'));
        }
      });
    });
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load recordings: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  // cachedItems holds the last successfully fetched recordings list so a
  // tab click or sort change can re-render synchronously without hitting
  // the network.
  var cachedItems = null;

  function render() {
    var root = document.getElementById('page-root');
    if (!root) return;
    if (!cachedItems) return;
    var active = getActiveStatus();
    var sort = getActiveSort();
    root.innerHTML = '';
    renderHeader(cachedItems);
    renderMetrics(root, cachedItems);
    renderTabs(root, cachedItems, active);
    renderTable(root, cachedItems, active, sort);
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
