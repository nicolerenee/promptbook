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
  //
  // view + mode form a 2x2: 'list'|'grid' × 'recordings'|'shows'.
  // shows[] is lazily populated the first time the user flips to mode
  // 'shows' and cached across mode flips so re-toggling doesn't
  // re-fetch /api/v1/shows. showsLoading / showsError mirror the
  // recordings loading / error fields so the view layer can branch on
  // the active mode without colliding state.
  library: {
    items: [],
    status: '',           // '' = All; otherwise lowercase storage.Status.
    sortKey: 'date',
    sortDir: 'desc',
    loading: true,
    error: null,
    view: 'list',         // 'list' | 'grid'.
    mode: 'recordings',   // 'recordings' | 'shows'.
    shows: [],
    showsLoading: false,
    showsError: null,
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
  //
  // Image picker fields (selectedPosterIndex, selectedBackdropIndex,
  // overlayOverride) mirror the server's persisted choice so the UI
  // can render the green-ring highlight + the override-vs-fallback
  // overlay-text input on first paint. overlayDraft is the
  // controlled-input string the user is currently typing — kept
  // separate so a half-typed label survives a redraw without
  // committing to the server. imageBusy disables every picker button
  // while a POST is in flight to prevent double-clicks; imageError
  // carries the inline failure string from the most recent POST.
  //
  // imageBusy + imageError are also reused by the upload flow
  // (utils/uploadPicker.js + Recording.js's runUpload) so a single
  // spinner / alert pair governs both selection and upload UX. The
  // multi-step upload-then-select flow toggles imageBusy explicitly
  // around the upload, then hands off to postPickerChoice for the
  // selection POST that promotes the upload to the active choice.
  recording: {
    id: null,
    loaded: null,
    loading: true,
    error: null,
    dangerBusy: false,
    dangerError: '',
    selectedPosterIndex: null,
    selectedBackdropIndex: null,
    overlayOverride: null,
    overlayDraft: '',
    // overlayDisabled mirrors the API field of the same name. When
    // true the renderer skips the playbill-style band and writes the
    // raw selected backdrop verbatim to rendered.jpg; the overlay
    // text editor renders disabled in that mode.
    overlayDisabled: false,
    imageBusy: false,
    imageError: null,
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
  // jobs powers the /jobs page. scheduled mirrors
  // /api/v1/jobs/scheduled; queue mirrors /api/v1/jobs/queue. timer
  // holds the setInterval handle so onremove can cancel polling on
  // route change. triggering tracks per-row "Run now" buttons that
  // are mid-POST so a re-render doesn't lose the disabled state.
  jobs: {
    scheduled: [],
    queue: [],
    loading: true,
    error: null,
    timer: null,
    triggering: {},
    triggerError: null,
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
