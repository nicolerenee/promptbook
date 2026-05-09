// Sidebar.js — left rail nav. Built on the DaisyUI `menu` component
// inside the responsive-collapsible-drawer-sidebar layout. Active
// route highlighting compares m.route.get() against each link's href.
//
// Nav groups (matching the legacy _sidebar.html groupings):
//   Library: Library / Wants / People
//   Activity: Queue / History / Sync
//   Reconcile: Mismatches
//   System: Settings
// Footer holds endpoint + rate placeholder + version (static for now;
// live rate-budget feed lands in a later wave).

import m from 'https://esm.sh/mithril@2.2.2';

// BrandIcon renders the playbill app-mark from the static brand
// directory. Two source files swap based on the current DaisyUI
// theme: light mode shows the navy-on-cream variant, dark mode the
// cream-on-navy. Theme detection reads the live `data-theme` attr
// from <html>, the same source theme.js writes when the user toggles.
function BrandIcon() {
  const theme = document.documentElement.getAttribute('data-theme') || 'night';
  // DaisyUI's "light" / "cream" / similar light themes use the
  // light asset; everything else (night, dark, dracula, ...) gets
  // the dark variant. Trivial heuristic — extend with an explicit
  // allowlist if we add more themes later.
  const isLight = /^(light|cream|cupcake|emerald|garden)$/i.test(theme);
  const src = isLight
    ? '/static/brand/appmark-light.svg'
    : '/static/brand/appmark-dark.svg';
  return m('img', {
    src,
    width: 28,
    height: 28,
    alt: '',
    'aria-hidden': 'true',
    class: 'block',
  });
}

// BrandWordmark renders the two-color "promptbook" lockup. "prompt"
// uses the dark/outline color, "book" uses the accent. CSS handles
// the theme swap via a custom-property-driven class so we don't have
// to special-case it here.
function BrandWordmark() {
  return m('span', {
    class: 'font-serif text-lg leading-none tracking-tight ' +
           'text-base-content',
  }, [
    m('span', { class: 'font-medium' }, 'prompt'),
    m('span', { class: 'font-bold text-primary' }, 'book'),
  ]);
}

// NAV_GROUPS is the static structure the sidebar renders. `match` is
// the predicate used to decide whether a given Mithril route counts as
// "this nav item is active" — for /people/:id we want the People entry
// highlighted, hence the prefix match for that one.
const NAV_GROUPS = [
  {
    label: 'Library',
    items: [
      { href: '/', name: 'Library', match: (p) => p === '/' },
      { href: '/wants', name: 'Wants', match: (p) => p === '/wants' },
      { href: '/people', name: 'People', match: (p) => p === '/people' || p.startsWith('/people/') },
    ],
  },
  {
    label: 'Activity',
    items: [
      { href: '/queue', name: 'Queue', match: (p) => p === '/queue' },
      { href: '/history', name: 'History', match: (p) => p === '/history' },
      { href: '/sync', name: 'Sync', match: (p) => p === '/sync' },
    ],
  },
  {
    label: 'Reconcile',
    items: [
      { href: '/mismatches', name: 'Mismatches', match: (p) => p === '/mismatches' || p === '/apply' },
    ],
  },
  {
    label: 'System',
    items: [
      { href: '/jobs', name: 'Jobs', match: (p) => p === '/jobs' },
      { href: '/settings', name: 'Settings', match: (p) => p === '/settings' },
    ],
  },
];

// navLink renders one menu item. The DaisyUI `menu-active` class marks
// the row matching the current route. Clicks go through m.route.set so
// pushState navigation stays inside the SPA.
function navLink(item, currentPath) {
  const active = item.match(currentPath);
  return m('li', m('a', {
    href: '#',
    class: active ? 'menu-active' : '',
    'aria-current': active ? 'page' : undefined,
    onclick: (ev) => {
      ev.preventDefault();
      m.route.set(item.href);
    },
  }, item.name));
}

const Sidebar = {
  view() {
    const path = m.route.get() || '/';
    return m('div', { class: 'flex min-h-full flex-col bg-base-200 w-64' }, [
      // Brand row.
      m('div', {
        class: 'flex items-center gap-2.5 px-4 py-4 text-base-content',
      }, [
        m('span', { class: 'inline-flex shrink-0' }, BrandIcon()),
        BrandWordmark(),
      ]),

      // Nav groups, each its own DaisyUI menu so the title/spacing
      // looks right.
      m('div', { class: 'flex-1 overflow-y-auto px-2' }, NAV_GROUPS.map((g) =>
        m('ul', { class: 'menu menu-sm w-full' }, [
          m('li', { class: 'menu-title' }, g.label),
          ...g.items.map((it) => navLink(it, path)),
        ])
      )),

      // Footer: endpoint + rate placeholder + version. Static text;
      // the rate-budget feed will be live-wired in a later wave. The
      // version is read from a global the SPA boot sets on first
      // load; falls back to "dev" when absent.
      m('div', {
        class: 'border-t border-base-300 px-4 py-3 text-xs text-base-content/60 space-y-1',
      }, [
        m('div', 'encora.it'),
        m('div', { class: 'flex items-center gap-2' }, [
          m('span', { class: 'opacity-60' }, 'rate'),
          m('span', { class: 'opacity-60' }, '—/30'),
        ]),
        m('div', { class: 'opacity-60' }, (window.__PB_VERSION__ || 'dev')),
      ]),
    ]);
  },
};

export default Sidebar;
