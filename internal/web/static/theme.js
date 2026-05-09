// theme.js — applies the user's saved theme + density to <html> on
// page load and exposes window.PB.toggleTheme / toggleDensity for the
// cog menu to wire to. The default values rendered by the layout
// (data-theme="light", data-density="cozy") are overwritten as soon as
// this file runs so a user's last choice persists across sessions.

(function () {
  'use strict';

  var STORE_KEY_THEME = 'pb.theme';
  var STORE_KEY_DENSITY = 'pb.density';

  function readStored(key, fallback) {
    try {
      var v = window.localStorage.getItem(key);
      if (v === 'light' || v === 'dark') return v;
      if (v === 'compact' || v === 'cozy' || v === 'comfy') return v;
      return fallback;
    } catch (_e) {
      return fallback;
    }
  }

  function applyTheme(theme) {
    var root = document.documentElement;
    root.setAttribute('data-theme', theme);
    try { window.localStorage.setItem(STORE_KEY_THEME, theme); } catch (_e) {}
  }

  function applyDensity(density) {
    var root = document.documentElement;
    root.setAttribute('data-density', density);
    try { window.localStorage.setItem(STORE_KEY_DENSITY, density); } catch (_e) {}
  }

  // Apply the stored values before first paint (script is in <head>
  // with `defer`, so the document is parsed but body isn't rendered yet).
  applyTheme(readStored(STORE_KEY_THEME, 'light'));
  applyDensity(readStored(STORE_KEY_DENSITY, 'cozy'));

  window.PB = window.PB || {};
  window.PB.toggleTheme = function () {
    var current = document.documentElement.getAttribute('data-theme') || 'light';
    applyTheme(current === 'dark' ? 'light' : 'dark');
  };
  window.PB.toggleDensity = function () {
    var order = ['compact', 'cozy', 'comfy'];
    var current = document.documentElement.getAttribute('data-density') || 'cozy';
    var idx = order.indexOf(current);
    var next = order[(idx + 1) % order.length];
    applyDensity(next);
  };
})();
