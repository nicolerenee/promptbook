// main.js — SPA entry point. Boots the Mithril router with the
// foundation route table: Library is the live pilot; everything else
// resolves to a generic StubPage placeholder.
//
// Routing notes:
//   - m.route.prefix = '' enables HTML5 pushState (no '#!' prefix), so
//     /recordings/123 is a real URL the server's catch-all serves.
//     This MUST be set before m.route() is called.
//   - The wrap() helper builds a Layout-wrapped resolver per route.
//     Mithril's `render` resolver hook receives the resolved vnode and
//     returns whatever should mount. We use it so Layout receives the
//     active page component as a prop and can render it inside the
//     drawer-content area.
//   - All routes share the same Layout shell — switching routes only
//     swaps the inner page, not the sidebar/topbar.

import m from 'https://esm.sh/mithril@2.2.2';
import Layout from './components/Layout.js';
import Library from './components/Library.js';
import Wants from './components/Wants.js';
import Sync from './components/Sync.js';
import Recording from './components/Recording.js';
import Queue from './components/Queue.js';
import History from './components/History.js';
import People from './components/People.js';
import Person from './components/Person.js';
import stubPage from './components/StubPage.js';
import { initTheme } from './components/Topbar.js';

// Apply the persisted (or default night) theme before the first
// render so users don't see a dark→light flash if they last picked
// light mode.
initTheme();

// HTML5 pushState. Set BEFORE m.route() per Mithril 2.x docs.
m.route.prefix = '';

// wrap takes a page component and returns a Mithril RouteResolver. The
// resolver renders Layout with the page baked into its attrs, so the
// drawer + topbar persist across route changes and only the inner
// content swaps.
function wrap(PageComponent) {
  return {
    render(vnode) {
      return m(Layout, {
        page: PageComponent,
        // Forward any matched path params (e.g. :id on
        // /recordings/:id) through Layout so the page sees them as
        // attrs. State.params on the resolved vnode carries the
        // current m.route.param() buttonshot.
        params: vnode.attrs,
      });
    },
  };
}

const root = document.getElementById('app');
if (!root) {
  // Hard fail — without #app there's no Mithril mount point. Log so
  // the user can spot the issue in devtools.
  console.error('promptbook: missing #app element; SPA cannot mount.');
} else {
  m.route(root, '/', {
    '/':                wrap(Library),
    '/recordings/:id':  wrap(Recording),
    '/wants':           wrap(Wants),
    '/sync':            wrap(Sync),
    '/queue':           wrap(Queue),
    '/people':          wrap(People),
    '/people/:id':      wrap(Person),
    '/history':         wrap(History),
    '/mismatches':      wrap(stubPage('Mismatches')),
    '/apply':           wrap(stubPage('Apply')),
    '/settings':        wrap(stubPage('Settings')),
  });
}
