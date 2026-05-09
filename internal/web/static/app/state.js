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
  // mismatches owns the reconciliation page state. `type` filters by
  // mismatch type (empty = All); `selected` is a Set of recording ids
  // ticked for batch apply. `applying` is true while a POST /apply is
  // in flight; `results` carries the per-action outcome on completion.
  mismatches: {
    items: [],
    type: '',
    selected: new Set(),
    loading: true,
    error: null,
    applying: false,
    results: null,        // null until first apply; array thereafter.
    applyError: null,
  },
  // settings holds the parsed /api/v1/settings payload. Read-only —
  // the page is documentation, not a control panel.
  settings: {
    data: null,
    loading: true,
    error: null,
  },
  // theme tracks the active DaisyUI theme name. Persisted to
  // localStorage by the toggle in Topbar so a refresh keeps the
  // preference.
  theme: 'night',
};

export default state;
