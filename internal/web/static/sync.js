// sync.js — fetches /api/v1/sync/runs and renders the Sync page.
//
// Mirrors the API-only page pattern established by library.js: the
// server template is a thin shell, this script populates the DOM from
// the JSON API. Renders a 4-up metric strip, a 14-run rate-budget
// sparkbar, and a table of the last 30 runs with expandable error
// detail rows.
//
// The "Sync now" header button is cosmetic for now — there's no
// POST /api/v1/sync endpoint yet, so the button carries a tooltip
// pointing users at the CLI.

(function () {
  'use strict';

  // Encora is hard-capped at 30 requests/minute; the rate-limit-budget
  // visualizations bucket against this ceiling.
  var RATE_CEILING = 30;
  // Sparkbar shows the most recent N runs left-to-right (oldest left).
  var SPARK_RUNS = 14;

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // relativeTime returns a short human-readable "Xm ago" / "Xh ago"
  // / "Xd ago" string for an ISO-8601 timestamp. Returns "—" when the
  // input is missing or unparseable.
  function relativeTime(iso) {
    if (!iso) return '—';
    var t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    var diff = Date.now() - t;
    if (diff < 0) diff = 0;
    var sec = Math.floor(diff / 1000);
    if (sec < 60) return sec + 's ago';
    var min = Math.floor(sec / 60);
    if (min < 60) return min + 'm ago';
    var hr = Math.floor(min / 60);
    if (hr < 24) return hr + 'h ago';
    var day = Math.floor(hr / 24);
    if (day < 30) return day + 'd ago';
    var mo = Math.floor(day / 30);
    if (mo < 12) return mo + 'mo ago';
    return Math.floor(mo / 12) + 'y ago';
  }

  // durationOf returns "{X.X}s" for a finished run, "—" when missing,
  // or "running…" when a run started but never finished.
  function durationOf(startedAt, finishedAt) {
    if (!startedAt) return '—';
    if (!finishedAt) return 'running…';
    var s = Date.parse(startedAt);
    var f = Date.parse(finishedAt);
    if (!isFinite(s) || !isFinite(f)) return '—';
    var ms = f - s;
    if (ms < 0) ms = 0;
    return (ms / 1000).toFixed(1) + 's';
  }

  // bucketColor maps a remaining rate-limit count to one of three CSS
  // variables. Errors > 0 always force the red bucket regardless of
  // the remaining count so the sparkbar surfaces failures visually.
  function bucketColor(remaining, errors) {
    if (errors > 0) return 'var(--accent)';
    var pct = (remaining / RATE_CEILING) * 100;
    if (pct >= 18) return 'var(--status-synced)';
    if (pct >= 6)  return 'var(--status-mismatch)';
    return 'var(--accent)';
  }

  function resultBadge(run) {
    var errs = run.error_count || 0;
    if (errs === 0) {
      return '<span class="pb-badge pb-status-synced">Synced</span>';
    }
    var ok = run.ok_count || 0;
    if (ok > 0) {
      return '<span class="pb-badge pb-status-mismatch">Retried</span>';
    }
    return '<span class="pb-badge pb-status-missing">Errored</span>';
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (sub) sub.textContent = 'Last 30 runs · ceiling 30 req/min';

    var actions = document.querySelector('[data-page-actions]');
    if (actions && !actions.dataset.rendered) {
      actions.dataset.rendered = '1';
      actions.innerHTML =
        '<button class="pb-btn pb-btn-ghost" type="button" data-action="configure" ' +
          'title="Sync configuration is in promptbook.yaml for now.">Configure</button>' +
        '<button class="pb-btn pb-btn-primary" type="button" data-action="sync-now" ' +
          'title="Use the CLI for now: promptbook collection sync">Sync now</button>';
      actions.querySelectorAll('button[data-action]').forEach(function (btn) {
        btn.addEventListener('click', function (e) { e.preventDefault(); });
      });
    }
    void items; // header copy is static; items reserved for future deltas
  }

  // mostRecentFinish picks the latest finished_at across all runs.
  // Runs in flight (finished_at == null) are skipped because we want
  // "last completed run", not "last started run".
  function mostRecentFinish(items) {
    var best = null;
    for (var i = 0; i < items.length; i++) {
      var f = items[i].finished_at;
      if (!f) continue;
      var t = Date.parse(f);
      if (!isFinite(t)) continue;
      if (best === null || t > best) best = t;
    }
    return best;
  }

  // errorsLast24h sums error_count across runs whose started_at is
  // within the last 24 hours. Falls back to finished_at if started_at
  // is missing.
  function errorsLast24h(items) {
    var cutoff = Date.now() - 24 * 60 * 60 * 1000;
    var n = 0;
    for (var i = 0; i < items.length; i++) {
      var stamp = items[i].started_at || items[i].finished_at;
      if (!stamp) continue;
      var t = Date.parse(stamp);
      if (!isFinite(t)) continue;
      if (t >= cutoff) n += items[i].error_count || 0;
    }
    return n;
  }

  function metricTile(label, num, sub, accent) {
    var numStyle = accent ? ' style="color:var(--accent)"' : '';
    return '<div class="pb-metric">' +
      '<div class="pb-metric-label">' + escapeHTML(label) + '</div>' +
      '<div class="pb-metric-num"' + numStyle + '>' + escapeHTML(String(num)) + '</div>' +
      '<div class="pb-metric-sub">' + escapeHTML(sub) + '</div>' +
    '</div>';
  }

  function renderMetrics(root, items) {
    var lastFinish = mostRecentFinish(items);
    var lastRunLabel = lastFinish ? relativeTime(new Date(lastFinish).toISOString()) : 'never';

    // The list is ordered by id DESC, so items[0] is the newest run
    // (whether finished or not). For the rate budget tile we want
    // whatever the most recent run reported — even an in-flight one.
    var newest = items.length > 0 ? items[0] : null;
    var rateBudget = newest ?
      (newest.rate_limit_remaining + '/' + RATE_CEILING) :
      ('—/' + RATE_CEILING);

    var errs24 = errorsLast24h(items);

    var html =
      '<div class="pb-metrics">' +
        metricTile('Last run', lastRunLabel,
          newest ? (newest.kind || 'all') + ' · run #' + newest.id : 'no runs yet') +
        metricTile('Rate budget', rateBudget, 'remaining of 30/min ceiling') +
        metricTile('Errors 24h', errs24,
          errs24 > 0 ? 'across recent runs' : 'all clean', errs24 > 0) +
        metricTile('Next run', 'manual', 'no scheduler configured') +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  // renderSparkbar emits a 14-bar SVG showing rate-limit budget over
  // the most recent N runs (newest right). Bar height is proportional
  // to remaining/30; color reflects the bucket from bucketColor().
  // Runs in flight (no finished_at) are still drawn so an in-progress
  // sync isn't invisible.
  function renderSparkbar(root, items) {
    // Take the newest SPARK_RUNS items (input is id DESC) and reverse
    // so the chart reads oldest→newest left→right.
    var slice = items.slice(0, SPARK_RUNS).reverse();
    var bars = [];
    var w = 100; // viewBox is 0..100; scaled responsively
    var h = 60;
    var gap = 4;
    var n = SPARK_RUNS;
    // Compute pixel widths in viewBox units.
    var totalGap = gap * (n - 1);
    var barW = (w - totalGap) / n;

    for (var i = 0; i < n; i++) {
      var run = slice[i];
      if (!run) {
        bars.push('<rect x="' + (i * (barW + gap)).toFixed(2) +
          '" y="' + (h - 2) + '" width="' + barW.toFixed(2) +
          '" height="2" fill="var(--rule)" rx="1"></rect>');
        continue;
      }
      var rem = Math.max(0, Math.min(RATE_CEILING, run.rate_limit_remaining || 0));
      var pct = rem / RATE_CEILING;
      var bh = Math.max(2, h * pct);
      var by = h - bh;
      var fill = bucketColor(rem, run.error_count || 0);
      var title = 'run #' + run.id + ' · ' + rem + '/' + RATE_CEILING +
        (run.error_count > 0 ? ' · ' + run.error_count + ' errors' : '');
      bars.push('<rect x="' + (i * (barW + gap)).toFixed(2) +
        '" y="' + by.toFixed(2) + '" width="' + barW.toFixed(2) +
        '" height="' + bh.toFixed(2) + '" fill="' + fill + '" rx="1">' +
        '<title>' + escapeHTML(title) + '</title></rect>');
    }

    var html = '<div class="pb-card" style="margin-bottom:18px">' +
      '<h3>Rate-limit budget across last 14 runs</h3>' +
      '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" ' +
      'style="width:100%;height:60px;display:block" role="img" ' +
      'aria-label="Rate-limit budget over the last 14 sync runs">' +
      bars.join('') +
      '</svg></div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderTable(root, items) {
    if (items.length === 0) {
      root.insertAdjacentHTML('beforeend',
        '<div class="pb-empty">' +
          'No sync runs yet. Run <span class="pb-mono">promptbook collection sync</span> ' +
          'or <span class="pb-mono">promptbook serve</span> to start mirroring.' +
        '</div>');
      return;
    }

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th>Started</th>' +
        '<th>Duration</th>' +
        '<th>Result</th>' +
        '<th>OK</th>' +
        '<th>Errors</th>' +
        '<th>Rate budget</th>' +
        '<th aria-label="Expand"></th>' +
      '</tr></thead><tbody>';

    items.forEach(function (run, idx) {
      var errs = run.error_count || 0;
      var ok = run.ok_count || 0;
      var rem = Math.max(0, Math.min(RATE_CEILING, run.rate_limit_remaining || 0));
      var pct = (rem / RATE_CEILING) * 100;
      var barColor = bucketColor(rem, errs);
      var hasErr = !!(run.error_text && run.error_text.length > 0);
      var rowID = 'pb-sync-run-' + (run.id != null ? run.id : 'idx' + idx);
      var startedISO = run.started_at || '';
      var startedTitle = startedISO ? ' title="' + escapeHTML(startedISO) + '"' : '';

      html += '<tr class="pb-row-link" data-runrow="' + rowID + '"' +
        (hasErr ? ' data-expandable="1"' : '') + '>' +
        '<td' + startedTitle + '>' + escapeHTML(relativeTime(startedISO)) +
          '<small style="display:block;color:var(--ink-4);font-family:var(--font-mono);font-size:11px">' +
          escapeHTML((run.kind || 'all') + ' · #' + (run.id != null ? run.id : '?')) +
          '</small></td>' +
        '<td class="pb-cell-mono">' + escapeHTML(durationOf(run.started_at, run.finished_at)) + '</td>' +
        '<td>' + resultBadge(run) + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(String(ok)) + '</td>' +
        '<td class="pb-cell-mono"' +
          (errs > 0 ? ' style="color:var(--accent)"' : '') + '>' +
          escapeHTML(String(errs)) + '</td>' +
        '<td>' +
          '<div style="display:flex;align-items:center;gap:8px">' +
            '<div class="pb-progress" style="width:80px">' +
              '<span style="width:' + pct.toFixed(1) + '%;background:' + barColor + '"></span>' +
            '</div>' +
            '<span class="pb-cell-mono">' + rem + '/' + RATE_CEILING + '</span>' +
          '</div>' +
        '</td>' +
        '<td class="pb-cell-mono" style="text-align:right;width:24px">' +
          (hasErr ? '<span aria-hidden="true">›</span>' : '') +
        '</td>' +
      '</tr>';

      if (hasErr) {
        html += '<tr class="pb-sync-detail" data-detail="' + rowID +
          '" hidden><td colspan="7" style="padding:14px">' +
          '<div style="font-size:11px;letter-spacing:0.06em;text-transform:uppercase;' +
          'color:var(--ink-4);margin-bottom:6px">Error</div>' +
          '<pre class="pb-json" style="white-space:pre-wrap;margin:0">' +
          escapeHTML(run.error_text) + '</pre>' +
          '</td></tr>';
      }
    });

    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);

    // Wire expand-on-click for rows that carry an error_text.
    root.querySelectorAll('tr[data-expandable="1"]').forEach(function (tr) {
      tr.addEventListener('click', function () {
        var key = tr.getAttribute('data-runrow');
        var detail = root.querySelector('tr.pb-sync-detail[data-detail="' + key + '"]');
        if (!detail) return;
        if (detail.hasAttribute('hidden')) detail.removeAttribute('hidden');
        else detail.setAttribute('hidden', '');
      });
    });
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load sync runs: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function render(items) {
    var root = document.getElementById('page-root');
    if (!root) return;
    root.innerHTML = '';
    renderHeader(items);
    renderMetrics(root, items);
    renderSparkbar(root, items);
    renderTable(root, items);
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="sync"]');
    if (!root) return;
    window.PB.api.get('/sync/runs')
      .then(function (body) {
        var items = (body && body.items) || [];
        render(items);
      })
      .catch(function (err) {
        renderError(root, err);
      });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
