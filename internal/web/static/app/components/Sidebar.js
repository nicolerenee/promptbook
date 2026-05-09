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
// directory. Both light and dark variants ship in the DOM; CSS in
// index.html toggles `display` based on the live `data-theme` attr,
// so swapping themes is instant + Mithril-redraw-free.
function BrandIcon() {
  const common = { width: 28, height: 28, alt: '', 'aria-hidden': 'true' };
  return m('span', { class: 'inline-flex shrink-0' }, [
    m('img', { ...common, class: 'pb-mark-light', src: '/static/brand/appmark-light.svg' }),
    m('img', { ...common, class: 'pb-mark-dark',  src: '/static/brand/appmark-dark.svg' }),
  ]);
}

// BrandWordmark renders the two-color "promptbook" lockup. "book" is
// always sky blue (#7FA8C9); "prompt" swaps between deep navy (light
// themes) and cream (dark themes). The actual color rules live in
// index.html's <style> block keyed off [data-theme] — the same hook
// the icon swap uses — so theme toggles update immediately without
// any JS redraw.
function BrandWordmark() {
  return m('span', { class: 'font-serif text-lg leading-none tracking-tight' }, [
    m('span', { class: 'pb-prompt font-medium' }, 'prompt'),
    m('span', { class: 'pb-book font-bold' }, 'book'),
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
        class: 'flex items-center gap-2.5 px-4 py-4',
      }, [
        BrandIcon(),
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
