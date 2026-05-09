// people.js — fetches /api/v1/people and renders the People list page.
//
// Pattern mirrors library.js: the server template ships an empty
// <div id="page-root" data-page="people"></div> shell; this script
// fetches data from the JSON API and populates the DOM.
//
// API gaps hit while implementing this page (TODO):
//   1. /api/v1/people doesn't return show names per performer, only
//      a recording_count. The design's grid card calls for a "truncated
//      show list" but resolving it would mean N+1 calls to
//      /api/v1/people/{id} on render — too expensive for the list view.
//      We render "View {N} credits →" instead and let the user click
//      through to the detail page.
//   2. The API doesn't currently expose headshot URLs. We render a
//      typographic monogram fallback (initials, italic Instrument Serif,
//      hashed-by-name palette). The /api/v1/people/{id} detail endpoint
//      DOES return a headshot_url (via stagemedia) — adding it to the
//      list payload would let us upgrade these monograms to real images.
//   3. Filter chips other than "All" are cosmetic in v1 — the API
//      doesn't yet expose "in library" / "in wants only" / "most
//      credits" / "recently added" facets. Marked visually disabled.
//   4. Sort defaults to name-ascending (matches the API's ORDER BY).
//      A future ?sort=name|credits param could flip it; not wired in v1.

(function () {
  'use strict';

  // ── HELPERS ─────────────────────────────────────────────────────────

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // monogram returns up to 2 initial characters from the performer's
  // name. "Eva Noblezada" → "EN", "Cher" → "C", empty → "?".
  function monogram(name) {
    if (!name) return '?';
    var parts = String(name).trim().split(/\s+/);
    var letters = '';
    for (var i = 0; i < parts.length && letters.length < 2; i++) {
      if (parts[i].length > 0) letters += parts[i][0];
    }
    return letters.toUpperCase() || '?';
  }

  // PALETTES are 6 deep-ink/cream pairs lifted from the design handoff
  // (pages-a.jsx). Each entry is [background, foreground] in oklch().
  // The hash-by-name picker keeps a performer's color stable across
  // page loads.
  var PALETTES = [
    ['oklch(0.32 0.05 60)',  'oklch(0.92 0.06 70)'],   // ink + cream
    ['oklch(0.40 0.10 25)',  'oklch(0.94 0.04 65)'],   // burgundy
    ['oklch(0.30 0.06 250)', 'oklch(0.88 0.06 80)'],   // midnight + gold
    ['oklch(0.45 0.10 145)', 'oklch(0.94 0.04 90)'],   // forest
    ['oklch(0.35 0.08 295)', 'oklch(0.92 0.06 75)'],   // plum
    ['oklch(0.55 0.14 60)',  'oklch(0.96 0.02 90)'],   // amber
  ];

  function paletteFor(name) {
    var s = String(name || '');
    var h = 0;
    for (var i = 0; i < s.length; i++) {
      h = (h * 31 + s.charCodeAt(i)) | 0;
    }
    return PALETTES[Math.abs(h) % PALETTES.length];
  }

  // ── VIEW STATE ──────────────────────────────────────────────────────

  function getView() {
    var p = new URLSearchParams(window.location.search);
    var v = (p.get('view') || '').toLowerCase();
    return v === 'list' ? 'list' : 'grid';
  }

  function setView(v) {
    var url = new URL(window.location.href);
    if (v === 'list') url.searchParams.set('view', 'list');
    else              url.searchParams.delete('view');
    window.history.pushState({}, '', url.toString());
    render();
  }

  // ── FILTER CHIPS ────────────────────────────────────────────────────

  // FILTER_CHIPS lists the cosmetic chip row shown above the cards.
  // Only "All" is wired in v1. The rest render with a small "—" badge
  // to communicate "filter coming soon" without throwing tooltips.
  // TODO: wire these to API facets once /api/v1/people supports
  // ?filter=in_library | in_wants | sort=credits | sort=recent.
  var FILTER_CHIPS = [
    { key: 'all',           label: 'All',            wired: true  },
    { key: 'in_library',    label: 'In library',     wired: false },
    { key: 'in_wants',      label: 'In wants only',  wired: false },
    { key: 'most_credits',  label: 'Most credits',   wired: false },
    { key: 'recently_added',label: 'Recently added', wired: false },
  ];

  // ── HEADSHOT (monogram fallback) ────────────────────────────────────

  function headshotHTML(name, size) {
    var pal = paletteFor(name);
    var fontPx = Math.round(size * 0.42);
    var style =
      'width:' + size + 'px;' +
      'height:' + size + 'px;' +
      'background:' + pal[0] + ';' +
      'color:' + pal[1] + ';' +
      'font-size:' + fontPx + 'px;';
    return '<div class="pb-headshot" style="' + style + '" aria-hidden="true">' +
      escapeHTML(monogram(name)) +
      '</div>';
  }

  // ── RENDERERS ───────────────────────────────────────────────────────

  function renderHeader(items) {
    var sub = document.querySelector('[data-page-sub]');
    if (!sub) return;
    var n = items.length;
    sub.textContent = n + ' performer' + (n === 1 ? '' : 's') +
                      ' in your library';
  }

  function renderToolbar(root, view) {
    var html =
      '<div class="pb-row" style="justify-content:space-between;margin-bottom:14px;flex-wrap:wrap;gap:12px">' +
        '<div class="pb-tabs" role="tablist" aria-label="View mode" style="margin-bottom:0">' +
          '<button class="pb-tab" role="tab" data-view="grid" aria-current="' +
            (view === 'grid' ? 'true' : 'false') + '"><span>Grid</span></button>' +
          '<button class="pb-tab" role="tab" data-view="list" aria-current="' +
            (view === 'list' ? 'true' : 'false') + '"><span>List</span></button>' +
        '</div>' +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('[data-view]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        setView(btn.getAttribute('data-view'));
      });
    });
  }

  function renderChips(root) {
    var html = '<div class="pb-row" style="gap:6px;margin-bottom:18px;flex-wrap:wrap">';
    FILTER_CHIPS.forEach(function (chip, i) {
      var active = (i === 0);
      var style =
        'border:1px solid var(--rule);' +
        'background:' + (active ? 'var(--bg-card)' : 'transparent') + ';' +
        'color:' + (active ? 'var(--ink)' : 'var(--ink-3)') + ';' +
        (chip.wired ? '' : 'opacity:0.6;cursor:default;');
      var disabledBadge = chip.wired ? '' :
        ' <span class="pb-tab-count" aria-label="filter coming soon">—</span>';
      html += '<button class="pb-tab" data-chip="' + escapeHTML(chip.key) + '" ' +
              (chip.wired ? '' : 'aria-disabled="true" ') +
              'style="' + style + '">' +
              '<span>' + escapeHTML(chip.label) + '</span>' +
              disabledBadge +
              '</button>';
    });
    html += '</div>';
    root.insertAdjacentHTML('beforeend', html);
    // Only "all" is wired. The rest are no-ops; we attach a click
    // handler that does nothing so the cursor doesn't change to a
    // pointer (per design: cursor stays default).
    root.querySelectorAll('[data-chip]').forEach(function (btn) {
      btn.addEventListener('click', function (e) { e.preventDefault(); });
    });
  }

  function renderEmpty(root) {
    var html =
      '<div class="pb-card" style="text-align:center">' +
        '<h3>No performers yet</h3>' +
        '<p class="pb-muted" style="margin:0 0 14px">' +
          'Sync your Encora collection to populate the cast directory.' +
        '</p>' +
        '<p class="pb-mono" style="font-size:12px;color:var(--ink-3);margin:0">' +
          'promptbook collection sync' +
        '</p>' +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderGrid(root, items) {
    var gridStyle =
      'display:grid;' +
      'grid-template-columns:repeat(auto-fill, minmax(200px, 1fr));' +
      'gap:14px;';
    var html = '<div style="' + gridStyle + '">';
    items.forEach(function (p) {
      var href = '/people/' + encodeURIComponent(p.performer_id);
      var idLabel = 'p-' + p.performer_id;
      var credits = (p.recording_count || 0);
      html +=
        '<a class="pb-card" href="' + escapeHTML(href) + '" ' +
           'style="padding:16px;text-decoration:none;color:inherit;display:block">' +
          '<div class="pb-row" style="gap:12px;align-items:flex-start">' +
            headshotHTML(p.name, 56) +
            '<div style="flex:1;min-width:0">' +
              '<div style="font-family:var(--font-display);font-style:italic;' +
                'font-size:19px;line-height:1.05;color:var(--ink);' +
                'overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
                escapeHTML(p.name || 'Unknown') +
              '</div>' +
              '<div class="pb-cell-id" style="margin-top:2px">' +
                escapeHTML(idLabel) +
              '</div>' +
            '</div>' +
          '</div>' +
          '<div class="pb-divider" style="margin:12px 0 10px"></div>' +
          '<div class="pb-row" style="justify-content:space-between;align-items:center">' +
            '<span class="pb-badge-square pb-status-synced">' +
              credits + ' credit' + (credits === 1 ? '' : 's') +
            '</span>' +
            '<span class="pb-mono" style="font-size:11px;color:var(--ink-3)">' +
              'View ' + credits + ' →' +
            '</span>' +
          '</div>' +
        '</a>';
    });
    html += '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderList(root, items) {
    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th style="width:48px"></th>' +
        '<th>Performer</th>' +
        '<th style="text-align:right">Credits</th>' +
        '<th style="width:60px"></th>' +
      '</tr></thead><tbody>';
    items.forEach(function (p) {
      var href = '/people/' + encodeURIComponent(p.performer_id);
      var credits = (p.recording_count || 0);
      var idLabel = 'p-' + p.performer_id;
      html +=
        '<tr class="pb-row-link" data-href="' + escapeHTML(href) + '">' +
          '<td>' + headshotHTML(p.name, 32) + '</td>' +
          '<td class="pb-cell-show" style="font-family:var(--font-display);font-style:italic;font-size:16px">' +
            escapeHTML(p.name || 'Unknown') +
            '<small>' + escapeHTML(idLabel) + '</small>' +
          '</td>' +
          '<td class="pb-cell-mono" style="text-align:right">' +
            credits +
          '</td>' +
          '<td class="pb-cell-mono" style="color:var(--ink-4)">View →</td>' +
        '</tr>';
    });
    if (items.length === 0) {
      html += '<tr><td colspan="4" class="pb-empty" style="padding:32px 14px">' +
              'No performers match.</td></tr>';
    }
    html += '</tbody></table></div>';
    root.insertAdjacentHTML('beforeend', html);
    root.querySelectorAll('tr.pb-row-link').forEach(function (tr) {
      tr.addEventListener('click', function () {
        window.location.href = tr.getAttribute('data-href');
      });
    });
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load people: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  // ── DRIVER ──────────────────────────────────────────────────────────

  // cachedItems holds the last successfully fetched people list so a
  // view-toggle click can re-render synchronously without hitting the
  // network. Mirrors library.js.
  var cachedItems = null;

  function render() {
    var root = document.getElementById('page-root');
    if (!root) return;
    if (!cachedItems) return;
    var view = getView();
    root.innerHTML = '';
    renderHeader(cachedItems);
    renderToolbar(root, view);
    renderChips(root);
    if (cachedItems.length === 0) {
      renderEmpty(root);
      return;
    }
    if (view === 'list') {
      renderList(root, cachedItems);
    } else {
      renderGrid(root, cachedItems);
    }
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="people"]');
    if (!root) return;
    // The API returns up to 50 by default; bump to 500 so the page
    // can render a single screen of results without paging UI in v1.
    // TODO: paginate once libraries grow past a few hundred performers.
    window.PB.api.get('/people?limit=500')
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
