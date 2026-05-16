// settings.js — fetches /api/v1/settings and renders the read-only
// configuration page. Mirrors the API-only page pattern: the server
// template is a thin shell, this script populates the DOM from the
// JSON payload.
//
// Read-only by design — config is owned by the YAML file and
// PROMPTBOOK_* env vars. The page surfaces the loaded values plus a
// theme toggle (the only client-side preference the UI persists).

(function () {
  'use strict';

  function escapeHTML(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // dash returns an em-dash placeholder for empty/zero string values so
  // the table doesn't print blank cells or literal "null".
  function dash(v) {
    if (v == null || v === '') return '<span style="color:var(--ink-4)">—</span>';
    return escapeHTML(v);
  }

  // mono wraps a value in the project's monospace cell style. Used for
  // paths, URLs, and templates where the text is structural.
  function mono(v) {
    if (v == null || v === '') return '<span style="color:var(--ink-4)">—</span>';
    return '<code class="pb-mono">' + escapeHTML(v) + '</code>';
  }

  // configuredBadge renders a green "configured" / muted "not
  // configured" label for boolean api_key_set fields. The wording
  // intentionally avoids exposing whether the key is empty vs. invalid;
  // the server only reports presence.
  function configuredBadge(set) {
    if (set) {
      return '<span class="pb-status-synced">configured</span>';
    }
    return '<span style="color:var(--ink-4)">not configured</span>';
  }

  // listVal renders a string slice as a comma-separated list of <code>
  // chunks, or an em-dash when empty. Used for library.incoming_dirs.
  function listVal(items) {
    if (!items || items.length === 0) {
      return '<span style="color:var(--ink-4)">—</span>';
    }
    return items.map(function (it) {
      return '<code class="pb-mono">' + escapeHTML(it) + '</code>';
    }).join(' ');
  }

  // sectionCard returns a `.pb-card` containing a 2-column table. rows
  // is an array of {label, value} where value is pre-rendered HTML.
  function sectionCard(title, rows) {
    var body = rows.map(function (r) {
      return '<tr><th scope="row" style="text-align:left;font-weight:500;' +
        'color:var(--ink-2);width:240px">' + escapeHTML(r.label) + '</th>' +
        '<td>' + r.value + '</td></tr>';
    }).join('');
    return '<div class="pb-card pb-card-pad-0" style="margin-bottom:18px">' +
      '<div style="padding:14px 18px 10px"><h3 style="margin:0">' +
      escapeHTML(title) + '</h3></div>' +
      '<table class="pb-table"><tbody>' + body + '</tbody></table>' +
      '</div>';
  }

  function renderEncora(cfg) {
    return sectionCard('Encora', [
      { label: 'Base URL', value: mono(cfg.base_url) },
      { label: 'API key', value: configuredBadge(cfg.api_key_set) },
      { label: 'User agent', value: mono(cfg.user_agent) },
      {
        label: 'Rate limit',
        value: '<span class="pb-mono">' +
          escapeHTML(String(cfg.rate_limit.requests_per_minute)) +
          '</span> requests/min · burst reserve ' +
          '<span class="pb-mono">' +
          escapeHTML(String(cfg.rate_limit.burst_reserve)) +
          '</span>',
      },
    ]);
  }

  function renderStorage(cfg) {
    return sectionCard('Storage', [
      { label: 'Database path', value: mono(cfg.database_path) },
    ]);
  }

  function renderLibrary(cfg) {
    return sectionCard('Library', [
      { label: 'Root', value: mono(cfg.root) },
      { label: 'Folder template', value: mono(cfg.folder_template) },
      { label: 'File template', value: mono(cfg.file_template) },
      { label: 'Incoming dirs', value: listVal(cfg.incoming_dirs) },
      { label: 'Watch interval', value: mono(cfg.watch_interval) },
    ]);
  }

  function renderServer(cfg) {
    return sectionCard('Server', [
      { label: 'Listen', value: mono(cfg.listen) },
    ]);
  }

  function renderOIDC(cfg) {
    return sectionCard('OIDC', [
      { label: 'Issuer', value: dash(cfg.issuer) },
      { label: 'Audience', value: dash(cfg.audience) },
      { label: 'JWKS refresh', value: mono(cfg.jwks_refresh) },
    ]);
  }

  function renderStagemedia(cfg) {
    return sectionCard('StageMedia', [
      { label: 'Base URL', value: mono(cfg.base_url) },
      { label: 'API key', value: configuredBadge(cfg.api_key_set) },
      { label: 'User agent', value: mono(cfg.user_agent) },
    ]);
  }

  function renderBuild(settings) {
    return sectionCard('Build', [
      { label: 'Version', value: mono(settings.version) },
      { label: 'Config source', value: mono(settings.config_source) },
    ]);
  }

  // currentTheme reads the persisted theme the same way theme.js does.
  // theme.js stores under "pb.theme"; we read it here without depending
  // on the script load order.
  function currentTheme() {
    try {
      var t = window.localStorage.getItem('pb.theme');
      if (t === 'light' || t === 'dark') return t;
    } catch (_e) {}
    return document.documentElement.getAttribute('data-theme') || 'light';
  }

  function renderTheme() {
    var theme = currentTheme();
    var html = '<div class="pb-card" style="margin-bottom:18px">' +
      '<h3 style="margin:0 0 10px">Theme</h3>' +
      '<div style="display:flex;align-items:center;gap:14px">' +
        '<div>Current theme: <strong data-settings-theme>' +
          escapeHTML(theme) + '</strong></div>' +
        '<button class="pb-btn pb-btn-ghost" type="button" ' +
          'data-action="toggle-theme">Toggle light / dark</button>' +
      '</div>' +
      '<p style="margin:10px 0 0;color:var(--ink-3);font-size:13px">' +
        'Theme is a per-browser preference; everything else on this page ' +
        'is read from <code class="pb-mono">promptbook.yaml</code> + ' +
        '<code class="pb-mono">PROMPTBOOK_*</code> env vars.' +
      '</p>' +
      '</div>';
    return html;
  }

  function renderError(root, err) {
    root.innerHTML = '<div class="pb-empty">Failed to load settings: ' +
      escapeHTML(err && err.message ? err.message : String(err)) + '</div>';
  }

  function render(root, settings) {
    root.innerHTML =
      renderEncora(settings.encora) +
      renderStorage(settings.storage) +
      renderLibrary(settings.library) +
      renderServer(settings.server) +
      renderOIDC(settings.server.oidc) +
      renderStagemedia(settings.stagemedia) +
      renderBuild(settings) +
      renderTheme();

    var btn = root.querySelector('button[data-action="toggle-theme"]');
    if (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        if (window.PB && window.PB.toggleTheme) {
          window.PB.toggleTheme();
        }
        var label = root.querySelector('[data-settings-theme]');
        if (label) label.textContent = currentTheme();
      });
    }
  }

  function init() {
    var root = document.querySelector('#page-root[data-page="settings"]');
    if (!root) return;
    window.PB.api.get('/settings')
      .then(function (body) { render(root, body || {}); })
      .catch(function (err) { renderError(root, err); });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
