// state.js — exported reactive state objects shared across components.
//
// Mithril doesn't ship a store; mutations on plain objects show up on
// the next m.redraw(). Each top-level key here owns one page's
// view-model so unrelated pages don't trip over each other. Mutators
// stay on the components that own them — this file only declares the
// shape + initial values.

export const state = {
  // library is the pilot page state. Mirrors the controls + cache the
  // legacy library.js maintained as module-locals.
  library: {
    items: [],
    status: '',           // '' = All; otherwise lowercase storage.Status.
    sortKey: 'date',
    sortDir: 'desc',
    loading: true,
    error: null,
  },
  // people is the /people index page state. q is the live search
  // query persisted to ?q=; sortKey/sortDir mirror Library's URL-
  // backed sort. items caches /api/v1/people responses across
  // navigations away and back.
  people: {
    items: [],
    q: '',
    sortKey: 'name',
    sortDir: 'asc',
    loading: true,
    error: null,
  },
  // person is the /people/:id detail page state. detail holds the
  // PersonDetail JSON; imgError flips to true when the headshot URL
  // fails to load so the next render falls back to the
  // avatar-placeholder. id tracks the active path param so onupdate
  // can detect a same-route id swap and re-fetch.
  person: {
    id: '',
    detail: null,
    sortKey: 'date',
    sortDir: 'desc',
    loading: true,
    error: null,
    imgError: false,
  },
  // theme tracks the active DaisyUI theme name. Persisted to
  // localStorage by the toggle in Topbar so a refresh keeps the
  // preference.
  theme: 'night',
};

export default state;
