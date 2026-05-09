// wants.js — fetches /api/v1/wants and renders the Wants ("shopping
// list") page. Mirrors the API-only page pattern established by
// library.js: the server template ships an empty
// <div id="page-root" data-page="wants"></div> shell and this script
// fetches data from the JSON API and populates the DOM.
//
// Design adjustments vs. design_handoff_promptbook_ui/README.md §9:
//
//   - Subhead drops the "{X} newly tradeable" claim. Promptbook has no
//     signal for NFT-expiry transitions yet, so we'd be lying. The
//     subhead reads "Your shopping list · {N} active wants".
//
//   - The "Featured · changes you should know about" section is
//     omitted. The design fakes that section with curated copy
//     ("now tradeable!", "filmed last week!") that needs signals we
//     don't track: there's no per-recording NFT-lift event, no global
//     popularity score, no "closing soon" feed. Once a curated /wants
//     featured endpoint exists (or we mine real signals), the section
//     comes back as a 3-card grid above the All-wants table.
//
//   - The "+ Add want" button is cosmetic for v1 with a "(use the
//     Encora UI for now)" tooltip; a future POST /api/v1/wants/{id}/add
//     endpoint will wire it up.
//
//   - The "added" column would render relative time off the wants
//     table's last_synced_at column, but the GET /api/v1/wants response
//     doesn't expose that field today. Cells render "—" until the API
//     surfaces it. The relativeTime helper is kept ready for that.

(function () {
  'use strict';

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

  // relativeTime renders an ISO-8601 timestamp as a coarse human string
  // ("3 days ago"). Held in reserve for the "added" column once the API
  // exposes wants.last_synced_at.
  function relativeTime(iso) {
    if (!iso) return '—';
    var t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    var diff = Math.max(0, Date.now() - t);
    var sec = Math.floor(diff / 1000);
    if (sec < 60)        return 'just now';
    var min = Math.floor(sec / 60);
    if (min < 60)        return min + (min === 1 ? ' minute ago' : ' minutes ago');
    var hr = Math.floor(min / 60);
    if (hr < 24)         return hr + (hr === 1 ? ' hour ago' : ' hours ago');
    var day = Math.floor(hr / 24);
    if (day < 14)        return day + (day === 1 ? ' day ago' : ' days ago');
    var wk = Math.floor(day / 7);
    if (wk < 8)          return wk + (wk === 1 ? ' week ago' : ' weeks ago');
    var mo = Math.floor(day / 30);
    if (mo < 12)         return mo + (mo === 1 ? ' month ago' : ' months ago');
    var yr = Math.floor(day / 365);
    return yr + (yr === 1 ? ' year ago' : ' years ago');
  }

  // sortByDateDesc orders items by date_full descending, with empty
  // dates sinking to the bottom. Matches the design's "most recent
  // wants first" intent.
  function sortByDateDesc(items) {
    var copy = items.slice();
    copy.sort(function (a, b) {
      var aHas = !!a.date_full;
      var bHas = !!b.date_full;
      if (aHas && !bHas) return -1;
      if (!aHas && bHas) return 1;
      if (!aHas && !bHas) return 0;
      if (a.date_full > b.date_full) return -1;
      if (a.date_full < b.date_full) return 1;
      return 0;
    });
    return copy;
  }

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (sub) {
      var n = items.length;
      sub.textContent = 'Your shopping list · ' + n +
                        (n === 1 ? ' active want' : ' active wants');
    }

    // The wants.html shell omits a [data-page-actions] slot, so inject
    // an actions container next to the title block ourselves.
    var pageH = document.querySelector('.pb-page-h');
    if (pageH && !pageH.querySelector('[data-wants-actions]')) {
      var actions = document.createElement('div');
      actions.className = 'pb-h-actions';
      actions.setAttribute('data-wants-actions', '');
      actions.innerHTML =
        '<button class="pb-btn" type="button" data-act="filter">Filter</button>' +
        '<button class="pb-btn pb-btn-primary" type="button" data-act="add" ' +
          'title="(use the Encora UI for now)">+ Add want</button>';
      pageH.appendChild(actions);
      // Both buttons are no-ops in v1; click handlers exist only to
      // swallow the event so a stray form submit can't escape.
      actions.querySelectorAll('button').forEach(function (b) {
        b.addEventListener('click', function (e) { e.preventDefault(); });
      });
    }
  }

  function renderTable(root, items) {
    var sorted = sortByDateDesc(items);

    // Section heading mirrors design_handoff_promptbook_ui/pages-b.jsx
    // ("All wants"). Inlined since there's no shared .pb-section-h
    // utility class yet.
    var html = '<h3 style="font-size:11px;font-weight:600;' +
      'letter-spacing:0.07em;text-transform:uppercase;' +
      'color:var(--ink-4);margin:0 0 10px">All wants</h3>' +
      '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th style="width:28px"></th>' +
        '<th>Recording</th>' +
        '<th>Date</th>' +
        '<th>Master</th>' +
        '<th>Encora format</th>' +
        '<th>Status</th>' +
        '<th>Added</th>' +
        '<th style="width:36px"></th>' +
      '</tr></thead><tbody>';

    sorted.forEach(function (it) {
      var subtitle = (it.tour ? escapeHTML(it.tour) + ' · ' : '') +
                     'enc-' + it.id;
      // last_synced_at would feed relativeTime here; legacy API shape
      // doesn't return it, so the column stays "—" for now.
      var added = it.last_synced_at ? relativeTime(it.last_synced_at) : '—';
      html +=
        '<tr>' +
          '<td class="pb-tr-status" style="--row-c:var(--status-wanted)">' +
            '<span class="pb-checkbox" aria-hidden="true"></span>' +
          '</td>' +
          '<td class="pb-cell-show">' + escapeHTML(it.show || '—') +
            '<small>' + subtitle + '</small></td>' +
          '<td class="pb-cell-mono">' +
            escapeHTML(smartDate(it.date_full, it.date_month_known, it.date_day_known)) +
          '</td>' +
          '<td>' + escapeHTML(it.master || '—') + '</td>' +
          '<td class="pb-cell-mono">—</td>' +
          '<td>' +
            '<span class="pb-badge-square pb-status-wanted">Wanted</span>' +
          '</td>' +
          '<td class="pb-cell-mono">' + escapeHTML(added) + '</td>' +
          '<td style="text-align:right">' +
            '<a href="/recordings/' + it.id + '" ' +
              'style="color:var(--ink-4);text-decoration:none" ' +
              'aria-label="Open recording ' + it.id + '">›</a>' +
          '</td>' +
        '</tr>';
    });

    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderEmpty(root) {
    root.insertAdjacentHTML('beforeend',
      '<div class="pb-empty">' +
        'No wants on Encora. Add some via the Encora UI and run ' +
        '<code>promptbook collection sync</code>.' +
      '</div>');
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load wants: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="wants"]');
    if (!root) return;

    window.PB.api.get('/wants')
      .then(function (body) {
        var items = (body && body.items) || [];
        root.innerHTML = '';
        renderHeader(items);
        if (items.length === 0) {
          renderEmpty(root);
          return;
        }
        renderTable(root, items);
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
