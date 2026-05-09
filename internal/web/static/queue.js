// queue.js — fetches /api/v1/queue and renders the manual import queue.
//
// API gap notes:
//   - The Match button POSTs to /api/v1/queue/{id}/import for high-
//     confidence rows. The Resolve… button is still cosmetic — picking
//     a recording from a different shape (search/autocomplete) is a
//     future UX flow.
//   - The design's "● ingesting" pulse + inline progress bar is
//     dropped — promptbook doesn't track in-flight ingest state for
//     queue rows. A future SSE or polling endpoint would let us add
//     it back.

(function () {
  'use strict';

  var CONF_META = {
    high:   { label: 'High',   cls: 'confidence-high' },
    medium: { label: 'Medium', cls: 'confidence-medium' },
    low:    { label: 'Low',    cls: 'confidence-low' },
  };

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function humanSize(bytes) {
    if (bytes == null || bytes === 0) return '—';
    var n = Number(bytes);
    if (!isFinite(n)) return '—';
    var GB = 1024 * 1024 * 1024;
    var MB = 1024 * 1024;
    var KB = 1024;
    if (n >= GB) return (n / GB).toFixed(2) + ' GB';
    if (n >= MB) return (n / MB).toFixed(1) + ' MB';
    if (n >= KB) return (n / KB).toFixed(1) + ' KB';
    return n + ' B';
  }

  function relativeTime(iso) {
    if (!iso) return '—';
    var t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    var deltaSec = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (deltaSec < 60) return deltaSec + ' sec ago';
    var deltaMin = Math.floor(deltaSec / 60);
    if (deltaMin < 60) return deltaMin + ' min ago';
    var deltaHr = Math.floor(deltaMin / 60);
    if (deltaHr < 24) return deltaHr + ' hr ago';
    var deltaDay = Math.floor(deltaHr / 24);
    if (deltaDay === 1) return 'yesterday';
    if (deltaDay < 7) return deltaDay + ' days ago';
    return new Date(t).toLocaleDateString();
  }

  function countMetrics(items) {
    var totals = { discovered: items.length, autoResolvable: 0, needsYou: 0 };
    items.forEach(function (it) {
      if (it.suggested_confidence === 'high') totals.autoResolvable++;
      else totals.needsYou++;
    });
    return totals;
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (sub) {
      sub.textContent = 'Watching incoming · polled every minute · ' +
        items.length + ' entr' + (items.length === 1 ? 'y' : 'ies');
    }
    var actions = document.querySelector('[data-page-actions]');
    if (!actions || actions.dataset.rendered) return;
    actions.dataset.rendered = '1';
    var hasAuto = items.some(function (it) { return it.suggested_confidence === 'high'; });
    var primaryAttrs = hasAuto ? '' : ' disabled';
    actions.innerHTML =
      '<button class="pb-btn pb-btn-ghost" type="button" title="(coming soon)">Re-scan</button>' +
      '<button class="pb-btn pb-btn-primary" type="button" title="(coming soon)"' + primaryAttrs +
        '>Import all auto-resolved</button>';
  }

  function metricTile(label, num, sub) {
    return '<div class="pb-metric">' +
      '<div class="pb-metric-label">' + escapeHTML(label) + '</div>' +
      '<div class="pb-metric-num">' + escapeHTML(String(num)) + '</div>' +
      '<div class="pb-metric-sub">' + escapeHTML(sub) + '</div>' +
    '</div>';
  }

  function renderMetrics(root, items) {
    var t = countMetrics(items);
    var html = '<div class="pb-metrics" style="grid-template-columns:repeat(3,1fr)">' +
      metricTile('Discovered', t.discovered, 'in queue') +
      '<div class="pb-metric" style="color:var(--status-synced)">' +
        '<div class="pb-metric-label">Auto-resolvable</div>' +
        '<div class="pb-metric-num">' + t.autoResolvable + '</div>' +
        '<div class="pb-metric-sub" style="color:inherit;opacity:0.75">high-confidence match</div>' +
      '</div>' +
      '<div class="pb-metric" style="color:var(--accent)">' +
        '<div class="pb-metric-label">Needs you</div>' +
        '<div class="pb-metric-num">' + t.needsYou + '</div>' +
        '<div class="pb-metric-sub" style="color:inherit;opacity:0.75">awaiting review</div>' +
      '</div>' +
    '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderTable(root, items) {
    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th style="width:36px"></th>' +
        '<th>Discovered</th>' +
        '<th>Confidence</th>' +
        '<th>File</th>' +
        '<th>Suggested match</th>' +
        '<th>Size</th>' +
        '<th style="width:120px;text-align:right"></th>' +
      '</tr></thead><tbody>';
    if (items.length === 0) {
      html += '<tr><td colspan="7" class="pb-empty" style="padding:32px 14px;text-align:center">' +
        'Queue is empty. Drop video files into your <code>library.incomingDirs</code> to see them here.' +
        '</td></tr>';
    }
    items.forEach(function (it) {
      var conf = CONF_META[it.suggested_confidence];
      var confBadge = conf ?
        '<span class="confidence-badge ' + conf.cls + '">' + escapeHTML(conf.label) + '</span>' :
        '<span class="pb-muted">—</span>';
      var match = it.suggested_recording_id ?
        '<a class="pb-cell-mono pb-link" href="/recordings/' + it.suggested_recording_id + '">' +
          'enc-' + it.suggested_recording_id + '</a>' :
        '<span class="pb-cell-mono pb-muted">— pick a recording —</span>';
      var path = escapeHTML(it.file_path || '');
      var pathCell = '<span class="pb-cell-mono" title="' + path + '" ' +
        'style="display:inline-block;max-width:480px;overflow:hidden;text-overflow:ellipsis;' +
        'white-space:nowrap;vertical-align:middle">' + path + '</span>';
      var actionBtn;
      if (it.suggested_confidence === 'high' && it.suggested_recording_id) {
        actionBtn = '<button class="pb-btn pb-btn-primary" type="button" ' +
          'data-queue-import="' + it.id + '" ' +
          'data-queue-path="' + escapeHTML(it.file_path || '') + '" ' +
          'data-queue-suggested="' + it.suggested_recording_id + '">Match</button>';
      } else {
        actionBtn = '<button class="pb-btn" type="button" ' +
          'title="(coming soon)" disabled>Resolve…</button>';
      }
      html += '<tr data-queue-row="' + it.id + '">' +
        '<td><input type="checkbox" class="pb-checkbox" disabled title="(coming soon)"></td>' +
        '<td class="pb-cell-mono">' + escapeHTML(relativeTime(it.discovered_at)) + '</td>' +
        '<td>' + confBadge + '</td>' +
        '<td>' + pathCell + '</td>' +
        '<td>' + match + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(humanSize(it.file_size_bytes)) + '</td>' +
        '<td style="text-align:right">' + actionBtn + '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';
    html += '<p class="pb-mono pb-muted" style="margin-top:14px;font-size:11.5px">' +
      'Files matching <code>[encora-NNNNN]</code> in their name auto-import on the next scan. ' +
      'Drop a <code>.encora-id</code> sidecar next to a video to add the ID without renaming.</p>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty" style="padding:32px;color:var(--status-missing)">' +
      'Failed to load queue: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  // errorMessage extracts a human-readable message from a fetch failure.
  // PB.api errors carry the response body on .body — usually a JSON
  // {message: "..."} from echo's NewHTTPError, occasionally a plain
  // string. Fall back to the synthetic .message if neither is useful.
  function errorMessage(err) {
    if (!err) return 'unknown error';
    if (err.body) {
      try {
        var parsed = JSON.parse(err.body);
        if (parsed && parsed.message) return String(parsed.message);
        if (parsed && parsed.error) return String(parsed.error);
      } catch (_) { /* not JSON; fall through */ }
      if (typeof err.body === 'string' && err.body.length < 240) return err.body;
    }
    return err.message || String(err);
  }

  // removeRow removes the queue row corresponding to queueID from the
  // DOM. No-op when the row is already gone (the table re-rendered or
  // the user navigated away).
  function removeRow(queueID) {
    var row = document.querySelector('[data-queue-row="' + queueID + '"]');
    if (row && row.parentNode) row.parentNode.removeChild(row);
  }

  function handleImportClick(btn) {
    var queueID = btn.getAttribute('data-queue-import');
    var path = btn.getAttribute('data-queue-path') || '';
    var suggested = btn.getAttribute('data-queue-suggested') || '';
    if (!queueID) return;
    var msg = 'Import ' + path + ' as recording enc-' + suggested + '?';
    if (!window.confirm(msg)) return;

    btn.setAttribute('disabled', 'disabled');
    btn.textContent = 'Importing…';

    window.PB.api.post('/queue/' + encodeURIComponent(queueID) + '/import', {})
      .then(function (resp) {
        if (resp && resp.ok) {
          removeRow(queueID);
          return;
        }
        btn.removeAttribute('disabled');
        btn.textContent = 'Match';
        window.alert('Import failed: ' + ((resp && resp.error) || 'unknown error'));
      })
      .catch(function (err) {
        // 503 (engine unconfigured) and 404 (queue entry vanished —
        // typically because a parallel scan already imported it) get
        // bespoke handling so the user sees something useful rather
        // than a raw error body.
        if (err && err.status === 503) {
          btn.removeAttribute('disabled');
          btn.textContent = 'Match';
          window.alert('Queue import is disabled — set library.root and ' +
            'PROMPTBOOK_ENCORA_APIKEY in config to enable.');
          return;
        }
        if (err && err.status === 404) {
          // Optimistically drop the row: the entry's already gone
          // upstream, so leaving it visible would invite a second click
          // that hits the same 404.
          removeRow(queueID);
          return;
        }
        btn.removeAttribute('disabled');
        btn.textContent = 'Match';
        window.alert('Import failed: ' + errorMessage(err));
      });
  }

  function bindImportButtons(root) {
    root.addEventListener('click', function (ev) {
      var btn = ev.target;
      while (btn && btn !== root && !btn.hasAttribute('data-queue-import')) {
        btn = btn.parentNode;
      }
      if (!btn || btn === root) return;
      ev.preventDefault();
      handleImportClick(btn);
    });
  }

  function init() {
    var root = document.getElementById('page-root');
    if (!root || root.getAttribute('data-page') !== 'queue') return;
    window.PB.api.get('/queue')
      .then(function (body) {
        var items = (body && body.items) || [];
        items.sort(function (a, b) {
          return Date.parse(b.discovered_at || 0) - Date.parse(a.discovered_at || 0);
        });
        root.innerHTML = '';
        renderHeader(items);
        renderMetrics(root, items);
        renderTable(root, items);
        bindImportButtons(root);
      })
      .catch(function (err) { renderError(root, err); });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
