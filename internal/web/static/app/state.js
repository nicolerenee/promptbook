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
  // wants holds the /api/v1/wants response + the current sort
  // selection. Status is fixed (every row is `wanted`) so unlike
  // Library there's no filter dimension to track.
  wants: {
    items: [],
    sortKey: 'wants_added',
    sortDir: 'desc',
    loading: true,
    error: null,
  },
  // sync mirrors /api/v1/sync/runs. Default order matches the API
  // (id desc, i.e. newest first); column clicks let the user re-sort
  // via the sortable-table pattern from Library.
  sync: {
    items: [],
    sortKey: 'started_at',
    sortDir: 'desc',
    loading: true,
    error: null,
  },
  // recording is the detail-page view-model. `id` is the currently
  // routed enc-NNNN; `loaded` holds the /api/v1/recordings/:id payload
  // (PascalCase fields because storage.LoadedRecording has no json
  // tags) merged with the lower-case server-side enrichment fields
  // (posters, nfo_content, nfo_modified_at). `dangerBusy` flips on
  // while a destructive POST is in flight, and `dangerError` carries
  // the inline error string when one comes back.
  recording: {
    id: null,
    loaded: null,
    loading: true,
    error: null,
    dangerBusy: false,
    dangerError: '',
  },
  // queue mirrors the legacy /static/queue.js view-model. items holds
  // the current /api/v1/queue rows; importing tracks per-row buttons
  // disabled while a POST is in flight so a re-render doesn't lose
  // the "Importing…" affordance.
  queue: {
    items: [],
    loading: true,
    error: null,
    importing: {},        // {[queueID]: true} while a POST is mid-flight.
  },
  // history mirrors the legacy /static/history.js view-model. kind is
  // the active filter tab; recordingID, when set, scopes the list to
  // a single recording's audit trail (driven by ?recording_id=).
  history: {
    items: [],
    kind: '',             // '' = All; otherwise lowercase HistoryKind* token.
    recordingID: null,
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
