// Settings.js — Mithril port of the legacy /static/settings.js.
//
// Read-only view of the loaded application configuration. The page is
// documentation, not a control panel — every value comes from the YAML
// config file plus PROMPTBOOK_* env vars, and the only client-side
// preference (theme) lives in the topbar's controller. This page just
// shows the active theme as a label.
//
// Section structure mirrors config.Config: Encora, Storage, Library,
// Server, OIDC, StageMedia, Build (version + config_source). Secrets
// are redacted server-side to a boolean (api_key_set), which we render
// as a green "configured" badge or a muted "not configured" label.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';

// dash returns an em-dash placeholder for missing/empty values so the
// table never prints a literally-empty cell. The class matches the
// rest of the SPA's "missing value" convention (opacity-40 muted).
function dashCell(v) {
  if (v == null || v === '') {
    return m('span', { class: 'opacity-40' }, '—');
  }
  return v;
}

// monoCell renders a value in a kbd badge for monospaced display —
// good for paths, URLs, and templates where the text is structural.
function monoCell(v) {
  if (v == null || v === '') {
    return m('span', { class: 'opacity-40' }, '—');
  }
  return m('kbd', { class: 'kbd kbd-sm' }, String(v));
}

// configuredBadge renders "configured" / "not configured" for the
// api_key_set booleans the server returns in place of the raw key.
// Wording avoids exposing whether the key is empty vs. invalid — the
// server only reports presence.
function configuredBadge(set) {
  if (set) {
    return m('span', { class: 'badge badge-success' }, 'configured');
  }
  return m('span', { class: 'opacity-40' }, 'not configured');
}

// listCell renders a string slice as space-separated kbd chunks. Used
// for library.incoming_dirs.
function listCell(items) {
  if (!items || items.length === 0) {
    return m('span', { class: 'opacity-40' }, '—');
  }
  return m('div', { class: 'flex flex-wrap gap-1' },
    items.map((it) => m('kbd', { class: 'kbd kbd-sm' }, String(it))));
}

// SectionCard wraps one config block in a DaisyUI card with a 2-column
// definition table. Rows is an array of [label, valueVnode] tuples.
function SectionCard(title, rows) {
  return m('div', { class: 'card bg-base-200 shadow-sm' }, [
    m('div', { class: 'card-body' }, [
      m('h3', { class: 'card-title text-lg' }, title),
      m('div', { class: 'overflow-x-auto' },
        m('table', { class: 'table table-sm' },
          m('tbody', rows.map(([label, value]) => m('tr', [
            m('th', {
              scope: 'row',
              class: 'font-medium opacity-80',
              style: 'width:200px',
            }, label),
            m('td', value),
          ])))
        )),
    ]),
  ]);
}

function renderEncora(cfg) {
  if (!cfg) return null;
  const rl = cfg.rate_limit || {};
  return SectionCard('Encora', [
    ['Base URL', monoCell(cfg.base_url)],
    ['API key', configuredBadge(cfg.api_key_set)],
    ['User agent', monoCell(cfg.user_agent)],
    ['Rate limit', m('span', [
      monoCell(String(rl.requests_per_minute || 0)),
      m('span', { class: 'mx-2 opacity-60' }, 'req/min · burst reserve'),
      monoCell(String(rl.burst_reserve || 0)),
    ])],
  ]);
}

function renderStorage(cfg) {
  if (!cfg) return null;
  return SectionCard('Storage', [
    ['Database path', monoCell(cfg.database_path)],
  ]);
}

function renderLibrary(cfg) {
  if (!cfg) return null;
  return SectionCard('Library', [
    ['Root', monoCell(cfg.root)],
    ['Folder template', monoCell(cfg.folder_template)],
    ['File template', monoCell(cfg.file_template)],
    ['Incoming dirs', listCell(cfg.incoming_dirs)],
    ['Watch interval', monoCell(cfg.watch_interval)],
  ]);
}

function renderServer(cfg) {
  if (!cfg) return null;
  return SectionCard('Server', [
    ['Listen', monoCell(cfg.listen)],
  ]);
}

function renderOIDC(cfg) {
  if (!cfg) return null;
  return SectionCard('OIDC', [
    ['Issuer', dashCell(cfg.issuer)],
    ['Audience', dashCell(cfg.audience)],
    ['JWKS refresh', monoCell(cfg.jwks_refresh)],
  ]);
}

function renderStagemedia(cfg) {
  if (!cfg) return null;
  return SectionCard('StageMedia', [
    ['Base URL', monoCell(cfg.base_url)],
    ['API key', configuredBadge(cfg.api_key_set)],
    ['User agent', monoCell(cfg.user_agent)],
  ]);
}

function renderBuild(settings) {
  return SectionCard('Build', [
    ['Version', monoCell(settings.version)],
    ['Config source', monoCell(settings.config_source)],
  ]);
}

// renderTheme shows the active theme name. The actual toggle lives in
// the topbar — this card just reflects current state via the shared
// state slot (kept in sync by Topbar.applyTheme).
function renderTheme() {
  const theme = state.theme ||
    document.documentElement.getAttribute('data-theme') ||
    'night';
  return m('div', { class: 'card bg-base-200 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('h3', { class: 'card-title text-lg' }, 'Theme'),
      m('div', { class: 'flex items-center gap-3' }, [
        m('span', { class: 'opacity-70' }, 'Current theme:'),
        m('span', { class: 'badge badge-neutral' }, theme),
      ]),
      m('p', { class: 'text-xs opacity-60 mt-2' }, [
        'Theme is a per-browser preference; toggle it from the topbar. ',
        'Everything else on this page is read from ',
        m('kbd', { class: 'kbd kbd-sm' }, 'promptbook.yaml'),
        ' + ',
        m('kbd', { class: 'kbd kbd-sm' }, 'PROMPTBOOK_*'),
        ' env vars.',
      ]),
    ]));
}

const Settings = {
  oninit() {
    const s = state.settings;
    s.loading = true;
    s.error = null;
    api.get('/settings').then((body) => {
      s.data = body || {};
      s.loading = false;
    }).catch((err) => {
      s.error = err;
      s.loading = false;
    });
  },

  view() {
    const s = state.settings;

    if (s.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading settings…');
    }
    if (s.error) {
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load settings: ' +
          (s.error.message || s.error)),
      ]);
    }

    const data = s.data || {};
    return m('div', { class: 'space-y-6' }, [
      m('header', [
        m('h1', { class: 'text-3xl font-semibold' }, 'Settings'),
        m('p', { class: 'text-sm opacity-70 mt-1' },
          'Read-only — managed via config file + ',
          m('kbd', { class: 'kbd kbd-sm' }, 'PROMPTBOOK_*'),
          ' env vars.'),
      ]),
      // Cards stack on small screens, grid on lg+ for a denser layout
      // that mirrors the legacy single-column flow at narrow widths.
      m('div', { class: 'grid gap-4 lg:grid-cols-2' }, [
        renderEncora(data.encora),
        renderStorage(data.storage),
        renderLibrary(data.library),
        renderServer(data.server),
        renderOIDC(data.server && data.server.oidc),
        renderStagemedia(data.stagemedia),
        renderBuild(data),
        renderTheme(),
      ]),
    ]);
  },
};

export default Settings;
