// Topbar.js — sticky top navbar above the main content area.
// Layout: drawer-open button (mobile) | search input (cosmetic stub) |
// theme toggle | settings shortcut.
//
// Theme toggle uses the DaisyUI `theme-controller` idiom: a checkbox
// flipping between two named themes. We bind two listeners — one for
// the controller's value semantics, one to persist the choice — and
// also write the chosen name to <html data-theme> so the change takes
// effect immediately (DaisyUI's CSS reads from data-theme, which the
// theme-controller pattern updates via :has() selectors but only on
// pages that actually contain the controlled element; persisting the
// chosen value to localStorage + applying it to <html> is the robust
// path across SPA route changes).

import m from 'https://esm.sh/mithril@2.2.2';
import state from '../state.js';

const THEME_STORAGE_KEY = 'pb.theme';
const THEMES = { dark: 'night', light: 'light' };

// applyTheme writes the chosen theme onto <html data-theme=...> and
// stashes it in localStorage. Called on every toggle + once at boot.
export function applyTheme(name) {
  const theme = name === THEMES.light ? THEMES.light : THEMES.dark;
  document.documentElement.setAttribute('data-theme', theme);
  state.theme = theme;
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // localStorage may be denied (private mode, embedded webview);
    // the page still works, the choice just won't persist.
  }
}

// initTheme runs once at SPA boot to pick up a previously persisted
// preference. Default is the night theme set on the static <html>.
export function initTheme() {
  let stored = null;
  try { stored = localStorage.getItem(THEME_STORAGE_KEY); } catch { /* ignore */ }
  if (stored === THEMES.light || stored === THEMES.dark) {
    applyTheme(stored);
  } else {
    applyTheme(THEMES.dark);
  }
}

// SearchIcon / SunIcon / MoonIcon / GearIcon — small inline SVGs
// matching the legacy topbar so the visual rhythm stays familiar.
function SearchIcon() {
  return m('svg', {
    class: 'h-[1em] opacity-60', xmlns: 'http://www.w3.org/2000/svg',
    viewBox: '0 0 24 24',
  }, m('g', {
    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    'stroke-width': '2.5', fill: 'none', stroke: 'currentColor',
  }, [
    m('circle', { cx: 11, cy: 11, r: 8 }),
    m('path', { d: 'm21 21-4.3-4.3' }),
  ]));
}
function SunIcon() {
  return m('svg', {
    'aria-label': 'sun', xmlns: 'http://www.w3.org/2000/svg',
    viewBox: '0 0 24 24', class: 'h-4 w-4',
  }, m('g', {
    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    'stroke-width': '2', fill: 'none', stroke: 'currentColor',
  }, [
    m('circle', { cx: 12, cy: 12, r: 4 }),
    m('path', { d: 'M12 2v2' }), m('path', { d: 'M12 20v2' }),
    m('path', { d: 'm4.93 4.93 1.41 1.41' }),
    m('path', { d: 'm17.66 17.66 1.41 1.41' }),
    m('path', { d: 'M2 12h2' }), m('path', { d: 'M20 12h2' }),
    m('path', { d: 'm6.34 17.66-1.41 1.41' }),
    m('path', { d: 'm19.07 4.93-1.41 1.41' }),
  ]));
}
function MoonIcon() {
  return m('svg', {
    'aria-label': 'moon', xmlns: 'http://www.w3.org/2000/svg',
    viewBox: '0 0 24 24', class: 'h-4 w-4',
  }, m('g', {
    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    'stroke-width': '2', fill: 'none', stroke: 'currentColor',
  }, m('path', { d: 'M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z' })));
}
function GearIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg', viewBox: '0 0 24 24',
    fill: 'none', stroke: 'currentColor', 'stroke-width': '1.6',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    class: 'h-4 w-4',
  }, [
    m('circle', { cx: 12, cy: 12, r: 3 }),
    m('path', { d: 'M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1-1.5 1.7 1.7 0 0 0-1.9.3l-.1.1A2 2 0 1 1 4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1 1.7 1.7 0 0 0-.3-1.9l-.1-.1A2 2 0 1 1 7 4.2l.1.1a1.7 1.7 0 0 0 1.9.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.9-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.9V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z' }),
  ]);
}

// HamburgerIcon for the mobile drawer toggle button. Hidden at lg+
// because the drawer is permanently open by then (lg:drawer-open on
// the layout root).
function HamburgerIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg', viewBox: '0 0 24 24',
    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    'stroke-width': '2', fill: 'none', stroke: 'currentColor',
    class: 'h-5 w-5',
  }, [
    m('path', { d: 'M3 6h18' }),
    m('path', { d: 'M3 12h18' }),
    m('path', { d: 'M3 18h18' }),
  ]);
}

const Topbar = {
  view() {
    const isLight = state.theme === THEMES.light;
    return m('nav', {
      class: 'navbar bg-base-300 border-b border-base-content/10',
    }, [
      // Mobile drawer-open button. The label-for-checkbox idiom is the
      // CSS-only DaisyUI drawer toggle; we don't manage the input
      // ourselves.
      m('label', {
        for: 'pb-drawer',
        'aria-label': 'open sidebar',
        class: 'btn btn-square btn-ghost lg:hidden',
      }, HamburgerIcon()),

      // Search stub — cosmetic for this wave; will get wired up in a
      // later wave. Kept in a flex-1 wrapper so the toggle + gear stay
      // pinned right.
      m('div', { class: 'flex-1 px-2' },
        m('label', { class: 'input input-sm max-w-md w-full' }, [
          SearchIcon(),
          m('input', {
            type: 'search',
            placeholder: 'Search recordings, people, history…',
            disabled: true,
          }),
          m('kbd', { class: 'kbd kbd-sm' }, '⌘K'),
        ])
      ),

      // Theme toggle — DaisyUI theme-controller checkbox. We pin its
      // checked state to the (already-applied) state.theme so the SPA
      // and the controller stay in sync across route changes.
      m('label', { class: 'swap swap-rotate btn btn-ghost btn-circle' }, [
        m('input', {
          type: 'checkbox',
          class: 'theme-controller',
          // The controller flips the theme via :has() when this
          // checkbox is in scope, but we also imperatively call
          // applyTheme() so persistence + <html data-theme> updates
          // happen regardless.
          value: THEMES.light,
          checked: isLight,
          'aria-label': 'Toggle theme',
          onchange: (ev) => {
            applyTheme(ev.target.checked ? THEMES.light : THEMES.dark);
          },
        }),
        m('span', { class: 'swap-on' }, SunIcon()),
        m('span', { class: 'swap-off' }, MoonIcon()),
      ]),

      // Settings shortcut.
      m('button', {
        type: 'button',
        class: 'btn btn-ghost btn-circle',
        'aria-label': 'Settings',
        onclick: () => m.route.set('/settings'),
      }, GearIcon()),
    ]);
  },
};

export default Topbar;
