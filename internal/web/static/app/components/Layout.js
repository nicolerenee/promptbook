// Layout.js — composes the sidebar drawer + topbar + page outlet.
// Built on the DaisyUI `responsive-collapsible-drawer-sidebar` layout
// snippet (drawer + lg:drawer-open). The drawer toggle is a CSS-only
// checkbox; clicks on the .drawer-overlay close it.
//
// `Page` is the resolved component for the current route (passed in
// via the m.route resolver in main.js); Layout renders <Page /> inside
// the .drawer-content area.

import m from 'https://esm.sh/mithril@2.2.2';
import Sidebar from './Sidebar.js';
import Topbar from './Topbar.js';

const Layout = {
  view(vnode) {
    const Page = vnode.attrs.page;
    const pageAttrs = vnode.attrs.params || {};
    return m('div', { class: 'drawer lg:drawer-open' }, [
      // The shared toggle checkbox the drawer-button (in Topbar) and
      // the drawer-overlay both target. Hidden via DaisyUI's
      // built-in offscreen styles.
      m('input', { id: 'pb-drawer', type: 'checkbox', class: 'drawer-toggle' }),

      // Main content column — Topbar pinned, then the route's page.
      m('div', { class: 'drawer-content flex flex-col min-h-screen' }, [
        m(Topbar),
        m('main', { class: 'flex-1 p-4 lg:p-6' },
          m(Page, pageAttrs),
        ),
      ]),

      // Sidebar drawer.
      m('div', { class: 'drawer-side z-20' }, [
        m('label', {
          for: 'pb-drawer',
          'aria-label': 'close sidebar',
          class: 'drawer-overlay',
        }),
        m(Sidebar),
      ]),
    ]);
  },
};

export default Layout;
