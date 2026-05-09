// person.js — fetches /api/v1/people/{id} and renders the person detail
// page. Mirrors the API-only page pattern established by library.js: the
// server template ships an empty shell with data-page="person" and the
// performer id stamped on data-person-id; this script populates the DOM.
//
// Notes for future API enrichment (informs a follow-up work unit):
//   - The /api/v1/people/{id} payload doesn't include per-recording state
//     (synced / wanted / missing / format_mismatch). To compute the "On
//     disk" and "Wants & missing" stats and render correct status pills
//     we cross-reference /api/v1/recordings?limit=200 once. When the
//     people endpoint grows a `state` field per credit we should drop
//     the side-call.
//   - Per-credit role text isn't on the API either; the cast_entries
//     join would need to surface in PersonRecording. Until then the
//     Role column renders "—".

(function () {
  'use strict';

  // STATUS_META keys mirror the lowercase tokens the /api/v1 endpoints
  // return ("synced", "format_mismatch", ...) so STATUS_META[item.status]
  // resolves directly without a re-mapping shim.
  var STATUS_META = {
    synced:          { label: 'Synced',          cls: 'pb-status-synced',   color: 'var(--status-synced)' },
    format_mismatch: { label: 'Format mismatch', cls: 'pb-status-mismatch', color: 'var(--status-mismatch)' },
    missing:         { label: 'Missing',         cls: 'pb-status-missing',  color: 'var(--status-missing)' },
    wanted:          { label: 'Wanted',          cls: 'pb-status-wanted',   color: 'var(--status-wanted)' },
    orphan:          { label: 'Orphan',          cls: 'pb-status-orphan',   color: 'var(--status-orphan)' },
  };

  // PALETTES mirrors the design-system monogram palettes (oklch ink/cream
  // pairs) so the typographic headshot fallback matches the people-list
  // page once it ships.
  var PALETTES = [
    ['oklch(0.32 0.05 60)',  'oklch(0.92 0.06 70)'],
    ['oklch(0.40 0.10 25)',  'oklch(0.94 0.04 65)'],
    ['oklch(0.30 0.06 250)', 'oklch(0.88 0.06 80)'],
    ['oklch(0.45 0.10 145)', 'oklch(0.94 0.04 90)'],
    ['oklch(0.35 0.08 295)', 'oklch(0.92 0.06 75)'],
    ['oklch(0.55 0.14 60)',  'oklch(0.96 0.02 90)'],
  ];

  // paletteFor picks a deterministic palette by hashing the name so a
  // performer always renders with the same colors.
  function paletteFor(name) {
    var h = 0;
    var s = name || '';
    for (var i = 0; i < s.length; i++) {
      h = (h * 31 + s.charCodeAt(i)) | 0;
    }
    return PALETTES[Math.abs(h) % PALETTES.length];
  }

  // monogram returns the up-to-2-letter initial used inside the headshot.
  // "Eva Noblezada" -> "EN", "Madonna" -> "M", "" -> "?".
  function monogram(name) {
    if (!name) return '?';
    var parts = String(name).trim().split(/\s+/);
    var out = '';
    for (var i = 0; i < parts.length && out.length < 2; i++) {
      if (parts[i].length > 0) out += parts[i].charAt(0).toUpperCase();
    }
    return out || '?';
  }

  // smartDate renders an ISO date with the precision the catalog
  // recorded:
  //   full date known           → YYYY-MM-DD
  //   day unknown, month known  → YYYY-MM
  //   month unknown             → YYYY
  //   no date at all            → —
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

  // hideStaticHeader removes the template's default page header
  // (`<div class="pb-page-h">Person.</div>`) so the breadcrumb + hero
  // can take over the top of the page. Using display:none keeps the
  // template untouched.
  function hideStaticHeader(root) {
    var h = root.parentElement && root.parentElement.querySelector('.pb-page-h');
    if (h) h.style.display = 'none';
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load person: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function renderNotFound(root) {
    var html = '';
    html += '<div class="pb-mono" style="font-size:11.5px;color:var(--ink-4);margin-bottom:14px">' +
      '<a class="pb-link" href="/people">People</a>' +
      '<span style="margin:0 6px">›</span>' +
      '<span>Unknown performer</span>' +
      '</div>';
    html += '<div class="pb-empty">No performer with that id is in your library.</div>';
    root.innerHTML = html;
  }

  // yearsActive returns "min–max" (en-dash) across recording date_full
  // values, or a single year, or "—" when nothing parses.
  function yearsActive(recordings) {
    var minY = null;
    var maxY = null;
    (recordings || []).forEach(function (r) {
      if (!r.date_full) return;
      var y = parseInt(String(r.date_full).substring(0, 4), 10);
      if (!isFinite(y)) return;
      if (minY === null || y < minY) minY = y;
      if (maxY === null || y > maxY) maxY = y;
    });
    if (minY === null) return '—';
    if (minY === maxY) return String(minY);
    return minY + '–' + maxY;
  }

  // statusFor returns the cross-referenced state for a recording id, or
  // "" if we couldn't load the recordings list (so the UI falls back to
  // a "—" cell rather than misrepresenting state).
  function statusFor(stateMap, id) {
    if (!stateMap) return '';
    var s = stateMap[String(id)];
    return s || '';
  }

  function renderBreadcrumb(root, name) {
    var html = '<div class="pb-mono" ' +
      'style="font-size:11.5px;color:var(--ink-4);margin-bottom:14px">' +
      '<a class="pb-link" href="/people">People</a>' +
      '<span style="margin:0 6px">›</span>' +
      '<span>' + escapeHTML(name) + '</span>' +
      '</div>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderHero(root, detail, stateMap) {
    var name = detail.name || '';
    var palette = paletteFor(name);
    var initial = monogram(name);
    var size = 160;
    var headshotStyle =
      'width:' + size + 'px;height:' + size + 'px;' +
      'background:' + palette[0] + ';color:' + palette[1] + ';' +
      'font-size:' + Math.round(size * 0.42) + 'px;';
    var headshot = '<div class="pb-headshot" style="' + headshotStyle + '">' +
      escapeHTML(initial) + '</div>';

    var creditCount = (typeof detail.recording_count === 'number') ?
      detail.recording_count :
      ((detail.recordings && detail.recordings.length) || 0);

    var onDisk;
    var wantsMissing;
    if (stateMap) {
      var diskN = 0;
      var wmN = 0;
      (detail.recordings || []).forEach(function (r) {
        var s = statusFor(stateMap, r.id);
        if (s === 'synced' || s === 'format_mismatch') diskN++;
        if (s === 'wanted' || s === 'missing') wmN++;
      });
      onDisk = String(diskN);
      wantsMissing = String(wmN);
    } else {
      onDisk = '—';
      wantsMissing = '—';
    }

    var years = yearsActive(detail.recordings);

    var kicker = 'PERFORMER · P-' + escapeHTML(String(detail.performer_id));

    var statHTML =
      stat('Library credits', String(creditCount)) +
      divider() +
      stat('On disk', onDisk) +
      divider() +
      stat('Wants & missing', wantsMissing) +
      divider() +
      stat('Years active', years);

    var html =
      '<div style="display:grid;grid-template-columns:160px 1fr;gap:28px;margin-bottom:26px">' +
        headshot +
        '<div style="padding-top:14px">' +
          '<div class="pb-mono pb-cell-id" ' +
            'style="font-size:11px;letter-spacing:0.08em;text-transform:uppercase">' +
            kicker +
          '</div>' +
          '<h1 class="pb-h1" style="font-size:64px;margin-top:4px">' +
            escapeHTML(name) +
          '</h1>' +
          '<div style="display:flex;gap:20px;margin-top:14px;font-size:13px;color:var(--ink-3)">' +
            statHTML +
          '</div>' +
        '</div>' +
      '</div>';

    root.insertAdjacentHTML('beforeend', html);
  }

  function stat(label, value) {
    return '<div>' +
      '<div class="pb-mono" ' +
        'style="font-size:10px;letter-spacing:0.08em;text-transform:uppercase;color:var(--ink-4)">' +
        escapeHTML(label) +
      '</div>' +
      '<div style="font-size:22px;color:var(--ink);font-family:var(--font-display);margin-top:2px">' +
        escapeHTML(value) +
      '</div>' +
    '</div>';
  }

  function divider() {
    return '<div style="width:1px;background:var(--rule)"></div>';
  }

  function renderAppearancesHeading(root) {
    var html = '<h3 ' +
      'style="font-size:11px;font-weight:600;letter-spacing:0.07em;' +
      'text-transform:uppercase;color:var(--ink-4);margin:0 0 10px">' +
      'Appearances in your library' +
      '</h3>';
    root.insertAdjacentHTML('beforeend', html);
  }

  function renderTable(root, recordings, stateMap) {
    if (!recordings || recordings.length === 0) {
      root.insertAdjacentHTML('beforeend',
        '<div class="pb-empty">' +
          'No recordings of this performer in your library yet.' +
        '</div>');
      return;
    }

    var html = '<div class="pb-table-wrap"><table class="pb-table">' +
      '<thead><tr>' +
        '<th>Status</th>' +
        '<th>Recording</th>' +
        '<th>Tour</th>' +
        '<th>Date</th>' +
        '<th>Role</th>' +
        '<th>Quality</th>' +
        '<th style="width:36px"></th>' +
      '</tr></thead><tbody>';

    recordings.forEach(function (r) {
      var status = statusFor(stateMap, r.id);
      var meta = STATUS_META[status];
      var rowC = meta ? meta.color : 'var(--ink-4)';
      var statusCell = meta ?
        '<span class="pb-badge pb-badge-square ' + meta.cls + '">' +
          escapeHTML(meta.label) + '</span>' :
        '<span class="pb-cell-mono">—</span>';
      var subtitle = 'enc-' + r.id;
      var tour = r.tour ? escapeHTML(r.tour) : '—';
      var date = escapeHTML(smartDate(r.date_full, r.date_month_known, r.date_day_known));

      html += '<tr class="pb-row-link" data-href="/recordings/' + r.id + '">' +
        '<td class="pb-tr-status" style="--row-c:' + rowC + '">' + statusCell + '</td>' +
        '<td class="pb-cell-show">' + escapeHTML(r.show || '—') +
          '<small>' + escapeHTML(subtitle) + '</small></td>' +
        '<td class="pb-cell-mono">' + tour + '</td>' +
        '<td class="pb-cell-mono">' + date + '</td>' +
        '<td class="pb-cell-mono">—</td>' +
        '<td class="pb-cell-mono">—</td>' +
        '<td style="color:var(--ink-4);text-align:right">›</td>' +
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

  function updateSubHeader(detail) {
    var sub = document.querySelector('[data-page-sub]');
    if (!sub) return;
    var n = (typeof detail.recording_count === 'number') ?
      detail.recording_count :
      ((detail.recordings && detail.recordings.length) || 0);
    sub.textContent = n + (n === 1 ? ' appearance' : ' appearances') +
      ' in your library';
  }

  function render(root, detail, stateMap) {
    hideStaticHeader(root);
    root.innerHTML = '';
    renderBreadcrumb(root, detail.name || 'Unknown performer');
    renderHero(root, detail, stateMap);
    renderAppearancesHeading(root);
    renderTable(root, detail.recordings || [], stateMap);
    updateSubHeader(detail);
  }

  // loadStateMap fetches the recordings list once and returns an
  // {id -> status} map. Resolves to null on error so the caller can
  // gracefully degrade ("—" stats and pills) rather than failing the
  // whole page.
  function loadStateMap() {
    return window.PB.api.get('/recordings?limit=200')
      .then(function (body) {
        var items = (body && body.items) || [];
        var map = {};
        items.forEach(function (it) {
          if (it && it.id != null) map[String(it.id)] = it.status || '';
        });
        return map;
      })
      .catch(function () { return null; });
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="person"]');
    if (!root) return;
    var id = root.getAttribute('data-person-id');
    if (!id) {
      renderError(root, new Error('missing performer id'));
      return;
    }

    Promise.all([
      window.PB.api.get('/people/' + encodeURIComponent(id)),
      loadStateMap(),
    ]).then(function (results) {
      render(root, results[0], results[1]);
    }).catch(function (err) {
      hideStaticHeader(root);
      if (err && err.status === 404) {
        renderNotFound(root);
        return;
      }
      renderError(root, err);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
