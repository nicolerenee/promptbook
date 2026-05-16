// StubPage.js — generic "Coming soon" placeholder. The factory takes a
// page name and returns a Mithril component that renders the name as
// the H1 plus a DaisyUI alert pointing the user at the JSON API in
// the meantime.

import m from 'https://esm.sh/mithril@2.2.2';

// InfoIcon mirrors the DaisyUI alert.info-color snippet's icon so the
// callout reads as an info-tone alert.
function InfoIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg', fill: 'none',
    viewBox: '0 0 24 24', class: 'h-6 w-6 shrink-0 stroke-current',
  }, m('path', {
    'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'stroke-width': '2',
    d: 'M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z',
  }));
}

// stubPage factory — returns a Mithril component bound to the supplied
// display name. Used in main.js's route table for every page that
// hasn't been ported yet (Recording, Wants, Sync, Queue, People, etc.).
export default function stubPage(name) {
  return {
    view() {
      return m('div', { class: 'space-y-4' }, [
        m('header', m('h1', { class: 'text-2xl font-semibold' }, name)),
        m('div', { role: 'alert', class: 'alert alert-info' }, [
          InfoIcon(),
          m('span', [
            'This page is being ported to the new UI. The legacy ',
            'implementation is still available as JSON via /api/v1/*.',
          ]),
        ]),
      ]);
    },
  };
}
