// recording.js — fetches /api/v1/recordings/{id} and renders the
// recording detail page. Mirrors the library.js pattern: data-page guard,
// PB.api.get, template strings, no React, no build step.
//
// The /api/v1/recordings/{id} endpoint returns the storage.LoadedRecording
// shape (see internal/storage/recordings.go). Field names are PascalCase
// because the Go types don't carry json tags. Notable fields used here:
//   - Recording.{Show, Tour, Master, NFT, Date, Metadata, Cast}
//   - InCollection, InWants, Format, CollectedAt
//   - Versions[].{FilePath, FileSizeBytes, Container, Quality, VideoCodec,
//     AudioCodec, FormatLabel}
//   - LocalFormatString
//   - Cast[].{Performer.{PerformerID,Name}, Character.{Name}, Status.{Label}}
//
// Server-side enrichment surfaced by /api/v1/recordings/{id}:
//   - posters[] — StageMedia poster URLs for the show. Empty when no
//     stagemedia client is configured or the show has none. We render
//     the first one as <img>; otherwise fall back to the typographic
//     mock.
//   - nfo_content / nfo_modified_at — content of the on-disk movie.nfo
//     next to the first version. Empty when no version exists or the
//     file hasn't been written yet; in that case the "NFO output" card
//     keeps its synthetic preview + PREVIEW badge.
//
// Things the API doesn't currently return (TODO API extensions):
//   - Real headshot URLs for performers. We render initials in a colored
//     circle instead.
//   - /api/v1/history doesn't accept a recording_id filter today, so we
//     fetch a wider window and filter client-side.
//
// NFT callout copy is the corrected version (NFT = "Not For Trade", not
// the design's "no-further-trade" — see library.js comment).

(function () {
  'use strict';

  // ─── Constants ────────────────────────────────────────────────────────

  var MONTHS_SHORT = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
                      'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var MONTHS_LONG = ['January', 'February', 'March', 'April', 'May', 'June',
                     'July', 'August', 'September', 'October', 'November', 'December'];

  // PALETTES are the deep-ink/cream pairs used by the typographic poster
  // mock + monogram headshots. Hash-of-name → palette index.
  var PALETTES = [
    ['oklch(0.32 0.05 60)',  'oklch(0.92 0.06 70)'],
    ['oklch(0.40 0.10 25)',  'oklch(0.94 0.04 65)'],
    ['oklch(0.30 0.06 250)', 'oklch(0.88 0.06 80)'],
    ['oklch(0.45 0.10 145)', 'oklch(0.94 0.04 90)'],
    ['oklch(0.35 0.08 295)', 'oklch(0.92 0.06 75)'],
    ['oklch(0.55 0.14 60)',  'oklch(0.96 0.02 90)'],
  ];

  function paletteFor(name) {
    var s = name || '';
    var h = 0;
    for (var i = 0; i < s.length; i++) h = ((h * 31) + s.charCodeAt(i)) | 0;
    return PALETTES[Math.abs(h) % PALETTES.length];
  }

  // ─── Inline icons (copied from design_handoff/components.jsx I object). ─

  var ICON_WARN =
    '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
    'stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M12 3 2.5 20h19L12 3z"/><path d="M12 10v4"/><path d="M12 17.5v.1"/></svg>';
  var ICON_CHECK =
    '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
    'stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="m5 12 4 4 10-10"/></svg>';
  var ICON_X =
    '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
    'stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="m6 6 12 12M18 6 6 18"/></svg>';
  var ICON_EXT =
    '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
    'stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" ' +
    'style="vertical-align:-1px;margin-left:4px">' +
    '<path d="M14 4h6v6"/><path d="M20 4 11 13"/>' +
    '<path d="M18 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h5"/></svg>';

  // ─── Helpers ──────────────────────────────────────────────────────────

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function smartDate(full, monthKnown, dayKnown) {
    if (!full) return '—';
    if (!monthKnown) return full.substring(0, 4);
    if (!dayKnown) {
      var mm = parseInt(full.substring(5, 7), 10);
      if (!isFinite(mm) || mm < 1 || mm > 12) return full;
      return MONTHS_SHORT[mm - 1] + ' ' + full.substring(0, 4);
    }
    return full;
  }

  // humanSize renders bytes as a binary-prefixed string (1 GB = 2^30).
  // app.css doesn't ship a helper so we inline one here.
  function humanSize(bytes) {
    var b = Number(bytes);
    if (!isFinite(b) || b <= 0) return '—';
    var units = ['B', 'KB', 'MB', 'GB', 'TB'];
    var u = 0;
    var n = b;
    while (n >= 1024 && u < units.length - 1) { n /= 1024; u++; }
    var precision = (u >= 3) ? 2 : (u >= 2 ? 1 : 0);
    return n.toFixed(precision) + ' ' + units[u];
  }

  function dirname(p) {
    if (!p) return '';
    var i = p.lastIndexOf('/');
    if (i < 0) return p;
    return p.substring(0, i + 1);
  }

  function basename(p) {
    if (!p) return '';
    var i = p.lastIndexOf('/');
    if (i < 0) return p;
    return p.substring(i + 1);
  }

  // formatNFTDate renders an ISO date or RFC3339 timestamp as
  // "Month D, YYYY" using locale-independent month names so the tests
  // and the user see the same string regardless of browser locale.
  function formatNFTDate(s) {
    if (!s) return '';
    var d = new Date(s);
    if (isNaN(d.getTime())) return s;
    return MONTHS_LONG[d.getMonth()] + ' ' + d.getDate() + ', ' + d.getFullYear();
  }

  // formatTS renders an RFC3339 timestamp as "YYYY-MM-DD HH:MM" for
  // mono columns. Falls back to the raw string on parse failure.
  function formatTS(s) {
    if (!s) return '—';
    var d = new Date(s);
    if (isNaN(d.getTime())) return s;
    function pad(n) { return n < 10 ? '0' + n : '' + n; }
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  // relativeTime renders an ISO timestamp as a coarse "5m ago" /
  // "3h ago" / "2d ago" string. Falls back to the YYYY-MM-DD prefix
  // when the diff is more than 30 days. Returns '' for empty input.
  function relativeTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    var diffSec = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    if (diffSec < 60) return diffSec + 's ago';
    if (diffSec < 3600) return Math.floor(diffSec / 60) + 'm ago';
    if (diffSec < 86400) return Math.floor(diffSec / 3600) + 'h ago';
    if (diffSec < 30 * 86400) return Math.floor(diffSec / 86400) + 'd ago';
    return d.toISOString().substring(0, 10);
  }

  // monogram returns up to 2 initials for the typographic headshot.
  function monogram(name) {
    if (!name) return '?';
    var parts = String(name).trim().split(/\s+/);
    if (parts.length === 1) return (parts[0][0] || '?').toUpperCase();
    return ((parts[0][0] || '') + (parts[parts.length - 1][0] || '')).toUpperCase();
  }

  // statusForRecording mirrors storage.ResolveStatus on the recording-
  // detail data we have. Used to colour the hero status pill.
  function statusForRecording(loaded) {
    var hasFile = (loaded.Versions && loaded.Versions.length > 0);
    if (loaded.InCollection) {
      if (!hasFile) return 'Missing';
      var local = loaded.LocalFormatString || '';
      var encora = loaded.Format || '';
      if (local && encora && local !== encora) return 'FormatMismatch';
      return 'Synced';
    }
    if (loaded.InWants) return 'Wanted';
    return 'Orphan';
  }

  var STATUS_LABEL = {
    Synced:         'Synced',
    FormatMismatch: 'Format mismatch',
    Missing:        'Missing',
    Wanted:         'Wanted',
    Orphan:         'Orphan',
  };
  var STATUS_CLASS = {
    Synced:         'pb-status-synced',
    FormatMismatch: 'pb-status-mismatch',
    Missing:        'pb-status-missing',
    Wanted:         'pb-status-wanted',
    Orphan:         'pb-status-orphan',
  };

  // qualityLabel collapses the per-version Quality + Codec into a single
  // "HD · 1080p HEVC"-style square-badge string for the hero meta row.
  function qualityLabel(loaded) {
    var v = (loaded.Versions || [])[0];
    if (!v) return '—';
    var q = v.Quality || '';
    var codec = v.VideoCodec || '';
    var parts = [];
    if (q) parts.push(q);
    if (codec) parts.push(codec);
    return parts.length ? parts.join(' ') : (v.Container || '—');
  }

  // ─── Render: hero ─────────────────────────────────────────────────────

  function renderBreadcrumb(r) {
    return '<div class="pb-mono" style="font-size:11.5px;color:var(--ink-4);margin-bottom:14px">' +
      '<a class="pb-link" href="/">Library</a>' +
      '<span style="margin:0 6px">›</span>' +
      '<span>' + escapeHTML(r.Show || '—') + '</span>' +
    '</div>';
  }

  // renderPoster prefers the first StageMedia poster URL when the API
  // returned one; otherwise it falls back to the typographic mock. The
  // mock is also the empty/missing-show fallback so a stagemedia outage
  // never blanks the page.
  function renderPoster(r, posters) {
    var first = (Array.isArray(posters) && posters.length > 0) ? posters[0] : '';
    if (first) {
      return '<div class="pb-poster" style="width:220px;height:330px">' +
        '<img class="poster" src="' + escapeHTML(first) + '" ' +
          'alt="' + escapeHTML(r.Show || '—') + ' poster" ' +
          'style="width:100%;height:100%;object-fit:cover;display:block">' +
      '</div>';
    }
    var pal = paletteFor(r.Show || 'Untitled');
    var year = '';
    if (r.Date && r.Date.FullDate) {
      year = r.Date.FullDate.substring(0, 4);
      if (r.Tour) year += ' · ' + String(r.Tour).toUpperCase();
    } else if (r.Tour) {
      year = String(r.Tour).toUpperCase();
    }
    var master = r.Master || '';
    return '<div class="pb-poster pb-poster-mock" ' +
      'style="width:220px;height:330px;background:' + pal[0] + ';color:' + pal[1] + '">' +
      '<div style="font-family:var(--font-mono);font-size:11px;letter-spacing:0.18em;' +
        'text-transform:uppercase;opacity:0.8">' +
        (master ? 'Master · ' + escapeHTML(master) : 'Bootleg recording') +
      '</div>' +
      '<div>' +
        '<div class="pm-show" style="font-size:48px">' + escapeHTML(r.Show || '—') + '</div>' +
        '<div class="pm-rule"></div>' +
        '<div class="pm-meta">' + escapeHTML(year) + '</div>' +
      '</div>' +
      '<div style="font-family:var(--font-mono);font-size:10px;letter-spacing:0.12em;' +
        'text-transform:uppercase;opacity:0.7">Promptbook · Library</div>' +
    '</div>';
  }

  function renderNFTCallout(loaded) {
    var nft = loaded.Recording.NFT || {};
    if (nft.NFTForever) {
      return '<div class="pb-callout">' +
        '<span class="pb-callout-icon">' + ICON_WARN + '</span>' +
        '<div>' +
          '<strong>Not For Trade — permanent.</strong>' +
          'Do not share or trade this recording.' +
        '</div>' +
      '</div>';
    }
    if (nft.NFTDate) {
      var when = new Date(nft.NFTDate);
      if (!isNaN(when.getTime()) && when.getTime() > Date.now()) {
        return '<div class="pb-callout">' +
          '<span class="pb-callout-icon">' + ICON_WARN + '</span>' +
          '<div>' +
            '<strong>Not For Trade — until ' + escapeHTML(formatNFTDate(nft.NFTDate)) + '.</strong>' +
            'Do not share or trade this recording until the date passes.' +
          '</div>' +
        '</div>';
      }
    }
    return '';
  }

  function renderHero(loaded) {
    var r = loaded.Recording;
    var status = statusForRecording(loaded);
    var subtitleParts = [];
    if (r.Tour) subtitleParts.push(r.Tour);
    var date = smartDate(
      r.Date && r.Date.FullDate,
      r.Date && r.Date.MonthKnown,
      r.Date && r.Date.DayKnown,
    );
    if (date && date !== '—') subtitleParts.push(date);
    if (r.Master) subtitleParts.push('master <span style="color:var(--ink-2)">' + escapeHTML(r.Master) + '</span>');
    var subtitle = subtitleParts.join(' · ');

    var html = '<div style="display:grid;grid-template-columns:220px 1fr;gap:28px;margin-bottom:22px">' +
      renderPoster(r, loaded.posters) +
      '<div style="display:flex;flex-direction:column;gap:14px;padding-top:6px">' +
        '<div>' +
          '<div class="pb-row" style="gap:10px;margin-bottom:6px">' +
            '<span class="pb-badge ' + STATUS_CLASS[status] + '"><span class="pb-badge-dot"></span>' +
              escapeHTML(STATUS_LABEL[status]) +
            '</span>' +
            '<span class="pb-badge-square pb-status-orphan">' + escapeHTML(qualityLabel(loaded)) + '</span>' +
            '<span class="pb-cell-id">enc-' + escapeHTML(String(r.ID)) + '</span>' +
          '</div>' +
          '<h1 class="pb-h1" style="font-size:56px">' + escapeHTML(r.Show || '—') + '</h1>' +
          (subtitle ? '<div style="margin-top:6px;font-size:14px;color:var(--ink-3);' +
                      'font-family:var(--font-mono)">' + subtitle + '</div>' : '') +
        '</div>' +
        renderNFTCallout(loaded) +
        renderDefs(loaded) +
      '</div>' +
    '</div>';
    return html;
  }

  function renderDefs(loaded) {
    var r = loaded.Recording;
    var meta = r.Metadata || {};
    var encoraURL = 'https://encora.it/recordings/' + encodeURIComponent(String(r.ID));
    var formatStr = loaded.InCollection ? (loaded.Format || '—') : '—';
    var gifting = meta.GiftingStatus || '—';
    var owners = (meta.OwnersCount != null) ? meta.OwnersCount : '—';
    var wanters = (meta.WantersCount != null) ? meta.WantersCount : '—';
    var cataloged = '—';
    if (loaded.InCollection && loaded.CollectedAt) {
      cataloged = formatNFTDate(loaded.CollectedAt) || loaded.CollectedAt;
    }
    var folder = '—';
    if (loaded.Versions && loaded.Versions.length > 0) {
      folder = dirname(loaded.Versions[0].FilePath) || '—';
    }
    return '<dl class="pb-defs">' +
      '<dt>Encora ID</dt><dd class="pb-mono">enc-' + escapeHTML(String(r.ID)) +
        ' <a class="pb-linkmono" href="' + escapeHTML(encoraURL) + '" target="_blank" rel="noopener noreferrer">' +
        'encora.it' + ICON_EXT + '</a></dd>' +
      '<dt>Format</dt><dd class="pb-mono">' + escapeHTML(formatStr) + '</dd>' +
      '<dt>Gifting</dt><dd>' + escapeHTML(gifting) + '</dd>' +
      '<dt>Owners / Wanters</dt><dd class="pb-mono">' +
        escapeHTML(String(owners)) + ' owners · ' +
        escapeHTML(String(wanters)) + ' wanters</dd>' +
      '<dt>Cataloged</dt><dd>' + escapeHTML(cataloged) + '</dd>' +
      '<dt>Folder</dt><dd class="pb-mono" style="font-size:12px">' + escapeHTML(folder) + '</dd>' +
    '</dl>';
  }

  // ─── Render: body left column ─────────────────────────────────────────

  function renderVersionsCard(loaded) {
    var versions = loaded.Versions || [];
    var html = '<div class="pb-card pb-card-pad-0">' +
      '<div style="padding:14px 18px 10px;display:flex;align-items:center;justify-content:space-between">' +
        '<h3 style="margin:0">Local versions · ' + versions.length + '</h3>' +
      '</div>';
    if (versions.length === 0) {
      var msg = loaded.InCollection
        ? 'No local files. The recording is in your collection but no version is registered.'
        : 'No local files registered for this recording.';
      html += '<div class="pb-empty" style="padding:28px 18px 32px;text-align:left">' +
              escapeHTML(msg) + '</div>';
      html += '</div>';
      return html;
    }
    html += '<table class="pb-table">' +
      '<thead><tr>' +
        '<th>Path</th>' +
        '<th>Format</th>' +
        '<th>Codec</th>' +
        '<th>Quality</th>' +
        '<th style="text-align:right">Size</th>' +
      '</tr></thead><tbody>';
    versions.forEach(function (v, idx) {
      var isPrimary = (idx === 0);
      var star = isPrimary
        ? '<span style="color:var(--accent);margin-right:6px">★</span>'
        : '';
      var name = basename(v.FilePath || '');
      var dir = dirname(v.FilePath || '');
      var pathTitle = escapeHTML(v.FilePath || '');
      var format = v.FormatLabel || v.Container || '—';
      var codec = v.VideoCodec || '—';
      if (v.AudioCodec) codec += ' / ' + v.AudioCodec;
      var quality = v.Quality || '—';
      html += '<tr>' +
        '<td class="pb-cell-mono" style="max-width:0;overflow:hidden;text-overflow:ellipsis;' +
          'white-space:nowrap" title="' + pathTitle + '">' +
          star + escapeHTML(name) +
          (dir ? '<small style="display:block;color:var(--ink-4);margin-top:2px;' +
                 'font-size:10.5px;overflow:hidden;text-overflow:ellipsis">' +
                 escapeHTML(dir) + '</small>' : '') +
        '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(format) + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(codec) + '</td>' +
        '<td class="pb-cell-mono">' + escapeHTML(quality) + '</td>' +
        '<td class="pb-cell-mono" style="text-align:right">' +
          escapeHTML(humanSize(v.FileSizeBytes)) + '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';
    return html;
  }

  function renderCastCard(loaded) {
    var cast = loaded.Cast || [];
    var html = '<div class="pb-card pb-card-pad-0">' +
      '<div style="padding:14px 18px 12px"><h3 style="margin:0">Cast · ' + cast.length + '</h3></div>';
    if (cast.length === 0) {
      html += '<div class="pb-empty" style="padding:0 18px 20px;text-align:left">' +
              'No cast recorded.</div></div>';
      return html;
    }
    html += '<div style="padding:0 18px 18px;display:grid;' +
      'grid-template-columns:repeat(2,1fr);gap:10px 24px">';
    cast.forEach(function (entry) {
      // TODO: real performer headshots aren't returned by the API yet —
      // render a typographic monogram fallback. Phase 5b admin overrides
      // will let us swap in real images.
      var perf = entry.Performer || {};
      var char = entry.Character || {};
      var status = entry.Status;
      var name = perf.Name || '—';
      var role = char.Name || '—';
      var pal = paletteFor(name);
      var pid = perf.PerformerID;
      var nameHTML;
      if (pid && pid > 0) {
        nameHTML = '<a class="pb-link" href="/people/' + encodeURIComponent(String(pid)) + '" ' +
                   'style="font-size:13px;font-weight:500;color:var(--ink)">' +
                   escapeHTML(name) + '</a>';
      } else {
        nameHTML = '<span style="font-size:13px;font-weight:500;color:var(--ink)">' +
                   escapeHTML(name) + '</span>';
      }
      var statusLine = '';
      if (status && status.Label) {
        statusLine = '<div class="pb-cell-mono" style="font-size:10.5px;color:var(--ink-4)">' +
                     escapeHTML(status.Label) + '</div>';
      }
      html += '<div class="pb-row" style="gap:12px;padding:6px 0;' +
        'border-top:1px solid var(--rule-2);align-items:flex-start">' +
        '<div class="pb-headshot" style="width:36px;height:36px;font-size:15px;' +
          'background:' + pal[0] + ';color:' + pal[1] + '">' +
          escapeHTML(monogram(name)) +
        '</div>' +
        '<div style="flex:1;min-width:0">' +
          nameHTML +
          '<div class="pb-mono" style="font-size:11.5px;color:var(--ink-3)">' +
            escapeHTML(role) +
          '</div>' +
          statusLine +
        '</div>' +
      '</div>';
    });
    html += '</div></div>';
    return html;
  }

  // ─── Render: body right column ────────────────────────────────────────

  function reconcilerRow(state, label, detail) {
    // state: 'ok' | 'no' | 'na'
    var icon, bg, color;
    if (state === 'ok') {
      icon = ICON_CHECK;
      bg = 'color-mix(in oklab, var(--status-synced) 18%, transparent)';
      color = 'var(--status-synced)';
    } else if (state === 'no') {
      icon = ICON_X;
      bg = 'color-mix(in oklab, var(--status-missing) 18%, transparent)';
      color = 'var(--status-missing)';
    } else {
      icon = '<span style="width:6px;height:6px;border-radius:99px;background:currentColor;display:inline-block"></span>';
      bg = 'var(--rule)';
      color = 'var(--ink-4)';
    }
    var detailHTML = detail
      ? '<div class="pb-mono" style="font-size:11px;color:var(--ink-4);margin-top:1px">' +
        escapeHTML(detail) + '</div>'
      : '';
    return '<div class="pb-row" style="gap:10px;align-items:flex-start">' +
      '<span style="width:18px;height:18px;border-radius:99px;background:' + bg +
        ';color:' + color + ';display:inline-flex;align-items:center;justify-content:center;' +
        'flex:0 0 auto">' + icon + '</span>' +
      '<div><div style="font-size:13px">' + escapeHTML(label) + '</div>' +
        detailHTML +
      '</div>' +
    '</div>';
  }

  function renderReconcilerCard(loaded) {
    var hasFile = (loaded.Versions && loaded.Versions.length > 0);
    var rows = [];
    rows.push(reconcilerRow(
      hasFile ? 'ok' : 'na',
      'File present locally',
      hasFile ? loaded.Versions.length + ' file(s)' : 'no version registered',
    ));
    rows.push(reconcilerRow(
      loaded.InCollection ? 'ok' : 'na',
      'In Encora collection',
      loaded.InCollection ? (loaded.Format || 'collected') : 'not collected',
    ));
    // InWants: green when NOT on wants (a recording on wants is an
    // in-progress acquisition, not a steady-state good).
    rows.push(reconcilerRow(
      loaded.InWants ? 'na' : 'ok',
      'On wants list',
      loaded.InWants ? 'shopping list' : 'not wanted',
    ));
    // Format match — only meaningful when both sides exist.
    if (loaded.InCollection) {
      var local = loaded.LocalFormatString || '';
      var encora = loaded.Format || '';
      var match = (local && encora && local === encora);
      var formatDetail = (local || '—') + ' / ' + (encora || '—');
      rows.push(reconcilerRow(
        match ? 'ok' : 'no',
        'Format matches Encora',
        formatDetail,
      ));
    }
    // NFO presence isn't tracked on this page — show as neutral.
    rows.push(reconcilerRow('na', 'NFO file written', '— (check on disk)'));
    return '<div class="pb-card">' +
      '<h3>Reconciler state</h3>' +
      '<div style="display:flex;flex-direction:column;gap:10px">' +
        rows.join('') +
      '</div>' +
    '</div>';
  }

  function renderNFOCard(loaded) {
    // When the API returns nfo_content, render the real on-disk file
    // and label it "from disk" with the file's mtime so the user
    // knows the page reflects what the writer last wrote. Falls back
    // to the synthetic preview when no NFO has been written yet.
    var nfoContent = loaded.nfo_content || '';
    if (nfoContent) {
      var note = 'from disk';
      var rel = relativeTime(loaded.nfo_modified_at);
      if (rel) note += ' · modified ' + rel;
      return '<div class="pb-card">' +
        '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">' +
          '<h3 style="margin:0">NFO output</h3>' +
          '<span class="pb-mono" style="font-size:10px;color:var(--ink-4)">' +
            escapeHTML(note) +
          '</span>' +
        '</div>' +
        '<pre class="pb-json" style="font-size:11px;margin:0">' + escapeHTML(nfoContent) + '</pre>' +
      '</div>';
    }
    var r = loaded.Recording;
    var meta = r.Metadata || {};
    var year = (r.Date && r.Date.FullDate) ? r.Date.FullDate.substring(0, 4) : '';
    var premiered = (r.Date && r.Date.FullDate && r.Date.MonthKnown && r.Date.DayKnown)
      ? r.Date.FullDate : '';
    var venue = meta.Venue || '';
    var preview = '<movie>\n' +
      '  <title>' + escapeHTML(r.Show || '') + '</title>\n' +
      (year ? '  <year>' + escapeHTML(year) + '</year>\n' : '') +
      (premiered ? '  <premiered>' + escapeHTML(premiered) + '</premiered>\n' : '') +
      (venue ? '  <studio>' + escapeHTML(venue) + '</studio>\n' : '') +
      '  <uniqueid type="encora" default="true">\n' +
      '    enc-' + escapeHTML(String(r.ID)) + '\n' +
      '  </uniqueid>\n' +
      (r.Tour ? '  <set><name>' + escapeHTML(r.Tour) + '</name></set>\n' : '') +
      '  ...';
    return '<div class="pb-card">' +
      '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">' +
        '<h3 style="margin:0">NFO output</h3>' +
        '<span class="pb-badge-square pb-status-wanted" style="font-size:10px">PREVIEW</span>' +
      '</div>' +
      '<pre class="pb-json" style="font-size:11px;margin:0">' + preview + '</pre>' +
    '</div>';
  }

  // ─── Render: danger zone ──────────────────────────────────────────────
  //
  // The Danger zone exposes destructive Encora pushes:
  //   - Remove from collection (when InCollection)
  //   - Remove from wants     (when InWants)
  //   - Add to wants          (when neither — "I want this")
  //
  // Each click goes through a typed-confirmation gate via window.prompt:
  // the user has to type the recording's enc-NNNN id before the POST
  // fires. This keeps the UI scope minimal (no bespoke modal framework)
  // while still giving the user a meaningful pause before a one-way
  // upstream action runs. Worth swapping for a styled in-page modal if
  // the rest of the app grows one — until then, prompt() matches the
  // codebase's "no fancy UI yet" tone.

  // errorMessage extracts a human-readable message from a fetch failure,
  // mirroring the helper in queue.js. PB.api errors carry the response
  // body on .body — the destructive endpoints return
  // {ok:false, error:"..."} on 409, so we surface .error preferentially,
  // then .message, then the synthetic err.message.
  function errorMessage(err) {
    if (!err) return 'unknown error';
    if (err.body) {
      try {
        var parsed = JSON.parse(err.body);
        if (parsed && parsed.error) return String(parsed.error);
        if (parsed && parsed.message) return String(parsed.message);
      } catch (_) { /* not JSON; fall through */ }
      if (typeof err.body === 'string' && err.body.length < 240) return err.body;
    }
    return err.message || String(err);
  }

  // dangerActionFor picks the single destructive action that makes
  // sense for the recording's current Encora state. Returns null when
  // none applies (shouldn't happen — Orphan recordings still get the
  // "Add to wants" branch).
  function dangerActionFor(loaded) {
    if (loaded.InCollection) {
      return {
        kind:    'remove_collection',
        label:   'Remove from collection',
        path:    '/encora/collection/' + loaded.Recording.ID + '/remove',
        confirm: 'This removes the recording from your Encora collection. ' +
                 'Local files stay on disk; only the upstream catalog row goes.',
      };
    }
    if (loaded.InWants) {
      return {
        kind:    'remove_wants',
        label:   'Remove from wants',
        path:    '/encora/wants/' + loaded.Recording.ID + '/remove',
        confirm: 'This removes the recording from your Encora wants list.',
      };
    }
    return {
      kind:    'add_wants',
      label:   'Add to wants',
      path:    '/encora/wants/' + loaded.Recording.ID + '/add',
      confirm: 'This adds the recording to your Encora wants list.',
    };
  }

  function renderDangerZone(loaded) {
    var action = dangerActionFor(loaded);
    if (!action) return '';
    // Buttons that remove are styled red; "add to wants" is constructive
    // so it gets the standard ghost button to avoid alarming the user.
    var isDestructive = (action.kind !== 'add_wants');
    var btnStyle = isDestructive
      ? 'border-color:var(--status-missing);color:var(--status-missing);background:transparent'
      : '';
    var btnAttrs = isDestructive ? ' data-danger-destructive="1"' : '';
    return '<div data-danger-zone="1" style="margin-top:28px;' +
      'padding-top:18px;border-top:1px solid var(--rule)">' +
      '<h3 style="margin:0 0 6px;font-size:13px;color:var(--ink-3);' +
        'letter-spacing:0.04em;text-transform:uppercase">Danger zone</h3>' +
      '<p style="margin:0 0 12px;font-size:12px;color:var(--ink-4);max-width:640px">' +
        escapeHTML(action.confirm) +
      '</p>' +
      '<button class="pb-btn" type="button" ' +
        'data-danger-action="' + escapeHTML(action.kind) + '" ' +
        'data-danger-path="' + escapeHTML(action.path) + '" ' +
        'data-danger-id="' + escapeHTML(String(loaded.Recording.ID)) + '"' +
        btnAttrs +
        (btnStyle ? ' style="' + btnStyle + '"' : '') +
        '>' + escapeHTML(action.label) + '</button>' +
      '<div data-danger-status style="margin-top:10px;font-size:12px;' +
        'color:var(--status-missing);min-height:1.2em"></div>' +
    '</div>';
  }

  // showDangerError surfaces a failure inline below the danger button
  // without an alert(). Mirrors the queue.js inline-error pattern.
  function showDangerError(root, msg) {
    var slot = root.querySelector('[data-danger-status]');
    if (slot) slot.textContent = msg;
  }

  // refreshRecording re-fetches the detail payload + history filter and
  // re-renders the page so the Danger zone reflects the new state. Used
  // after a successful destructive POST. Keeping this scoped here (vs.
  // calling init() recursively) avoids re-binding event listeners on
  // already-rendered elements.
  function refreshRecording(root, id) {
    return window.PB.api.get('/recordings/' + encodeURIComponent(id))
      .then(function (loaded) {
        return window.PB.api.get('/history?limit=50')
          .then(function (body) {
            var rid = Number(id);
            var items = ((body && body.items) || [])
              .filter(function (e) { return Number(e.recording_id) === rid; })
              .slice(0, 3);
            return { loaded: loaded, history: items };
          })
          .catch(function () { return { loaded: loaded, history: [] }; });
      })
      .then(function (data) { render(root, data.loaded, data.history); });
  }

  function handleDangerClick(root, btn) {
    var path = btn.getAttribute('data-danger-path');
    var rid = btn.getAttribute('data-danger-id');
    if (!path || !rid) return;
    showDangerError(root, '');

    // Typed-confirmation: user must type "enc-NNNN" before the POST
    // fires. window.prompt returns null on cancel, '' on empty, the
    // typed string otherwise.
    var token = 'enc-' + rid;
    var typed = window.prompt(
      'Type "' + token + '" to confirm. This action pushes upstream to Encora.',
      '',
    );
    if (typed == null) return;
    if (String(typed).trim() !== token) {
      showDangerError(root, 'Confirmation text did not match — nothing changed.');
      return;
    }

    var origLabel = btn.textContent;
    btn.setAttribute('disabled', 'disabled');
    btn.textContent = 'Working…';

    window.PB.api.post(path, {})
      .then(function (resp) {
        if (resp && resp.ok) {
          // refreshRecording re-runs render() which rebuilds the
          // danger zone for the new state, so we don't need to reset
          // the button — it gets replaced wholesale.
          return refreshRecording(root, rid);
        }
        btn.removeAttribute('disabled');
        btn.textContent = origLabel;
        showDangerError(root,
          'Failed: ' + ((resp && resp.error) || 'unknown error'));
      })
      .catch(function (err) {
        btn.removeAttribute('disabled');
        btn.textContent = origLabel;
        if (err && err.status === 503) {
          showDangerError(root,
            'Encora client not configured — set PROMPTBOOK_ENCORA_APIKEY ' +
            'on the server to enable upstream writes.');
          return;
        }
        if (err && err.status === 429) {
          showDangerError(root,
            'Rate-limited by Encora. Wait a minute and try again.');
          return;
        }
        showDangerError(root, 'Failed: ' + errorMessage(err));
      });
  }

  // bindDangerZone attaches a single delegated click handler on the
  // recording root. Re-rendering the page replaces inner HTML, but the
  // root element itself is stable so one binding survives refreshes.
  function bindDangerZone(root) {
    if (root.dataset.dangerBound) return;
    root.dataset.dangerBound = '1';
    root.addEventListener('click', function (ev) {
      var btn = ev.target;
      while (btn && btn !== root && !btn.hasAttribute('data-danger-action')) {
        btn = btn.parentNode;
      }
      if (!btn || btn === root) return;
      ev.preventDefault();
      handleDangerClick(root, btn);
    });
  }

  function renderActivityCard(items) {
    var html = '<div class="pb-card">' +
      '<h3>Activity</h3>';
    if (!items || items.length === 0) {
      html += '<div class="pb-empty" style="padding:6px 0;text-align:left">No activity yet.</div></div>';
      return html;
    }
    html += '<ul style="margin:0;padding:0;list-style:none;display:flex;' +
      'flex-direction:column;gap:10px;font-size:12.5px">';
    items.forEach(function (e, idx) {
      var dotColor = idx === 0 ? 'var(--status-synced)'
        : (idx === 1 ? 'var(--status-wanted)' : 'var(--ink-4)');
      html += '<li class="pb-row" style="align-items:flex-start">' +
        '<span style="width:8px;height:8px;border-radius:99px;background:' + dotColor +
          ';margin-top:6px;flex:0 0 auto"></span>' +
        '<div>' +
          '<div>' + escapeHTML(e.summary || e.kind || 'event') + '</div>' +
          '<div class="pb-mono" style="color:var(--ink-4);font-size:11px">' +
            escapeHTML(formatTS(e.occurred_at)) +
            (e.kind ? ' · ' + escapeHTML(e.kind) : '') +
          '</div>' +
        '</div>' +
      '</li>';
    });
    html += '</ul></div>';
    return html;
  }

  // ─── Page-level render orchestration ──────────────────────────────────

  function renderHeaderSub(loaded) {
    var sub = document.querySelector('[data-page-sub]');
    if (!sub) return;
    var r = loaded.Recording;
    var bits = ['enc-' + r.ID];
    if (r.Show) bits.push(r.Show);
    var status = statusForRecording(loaded);
    bits.push(STATUS_LABEL[status].toLowerCase());
    sub.textContent = bits.join(' · ');
  }

  function renderEmpty(root, message) {
    root.innerHTML = '<div class="pb-empty">' + escapeHTML(message) + '</div>';
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load recording: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function render(root, loaded, history) {
    renderHeaderSub(loaded);
    var html = renderBreadcrumb(loaded.Recording) +
      renderHero(loaded) +
      '<div style="display:grid;grid-template-columns:1.6fr 1fr;gap:18px">' +
        '<div style="display:flex;flex-direction:column;gap:18px">' +
          renderVersionsCard(loaded) +
          renderCastCard(loaded) +
        '</div>' +
        '<div style="display:flex;flex-direction:column;gap:18px">' +
          renderReconcilerCard(loaded) +
          renderNFOCard(loaded) +
          renderActivityCard(history) +
        '</div>' +
      '</div>' +
      renderDangerZone(loaded);
    root.innerHTML = html;
  }

  // ─── Init ─────────────────────────────────────────────────────────────

  function init() {
    var root = document.querySelector('#page-root[data-page="recording"]');
    if (!root) return;
    var id = root.getAttribute('data-recording-id');
    if (!id) {
      renderEmpty(root, 'Missing recording id.');
      return;
    }

    window.PB.api.get('/recordings/' + encodeURIComponent(id))
      .then(function (loaded) {
        // TODO(API): /api/v1/history doesn't accept ?recording_id=
        // today, so we fetch a wider window and filter client-side.
        // When the endpoint grows the param, switch to a 3-item fetch.
        return window.PB.api.get('/history?limit=50')
          .then(function (body) {
            var rid = Number(id);
            var items = ((body && body.items) || [])
              .filter(function (e) { return Number(e.recording_id) === rid; })
              .slice(0, 3);
            return { loaded: loaded, history: items };
          })
          .catch(function () {
            return { loaded: loaded, history: [] };
          });
      })
      .then(function (data) {
        render(root, data.loaded, data.history);
        bindDangerZone(root);
      })
      .catch(function (err) {
        if (err && err.status === 404) {
          var sub = document.querySelector('[data-page-sub]');
          if (sub) sub.textContent = 'not found';
          renderEmpty(root, 'Recording not found.');
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
