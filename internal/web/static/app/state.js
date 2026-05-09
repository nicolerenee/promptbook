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
  // theme tracks the active DaisyUI theme name. Persisted to
  // localStorage by the toggle in Topbar so a refresh keeps the
  // preference.
  theme: 'night',
};

export default state;
