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
    sortKey: 'recording',
    sortDir: 'asc',
    loading: true,
    error: null,
    view: 'list',         // 'list' | 'grid'.
    mode: 'recordings',   // 'recordings' | 'shows'.
    shows: [],
    showsLoading: false,
    showsError: null,
    // Pagination — recordings + shows each track their own offset +
    // total because flipping mode swaps the dataset entirely. limit
    // is hardcoded to 50 in the API layer; the SPA mirrors it here so
    // the indicator math agrees with the server's slice.
    offset: 0,
    total: 0,
    showsOffset: 0,
    showsTotal: 0,
    limit: 50,
  },
  // wants holds the /api/v1/wants response + the current sort
  // selection. Status is fixed (every row is `wanted`) so unlike
  // Library there's no filter dimension to track. offset / total /
  // limit drive the shared Pagination component.
  wants: {
    items: [],
    sortKey: 'wants_added',
    sortDir: 'desc',
    loading: true,
    error: null,
    offset: 0,
    total: 0,
    limit: 50,
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
    // pickerOpen flips true when the "Edit images" header button opens
    // the image-picker modal. The modal owns a <dialog> ref and syncs
    // showModal()/close() to this flag via Mithril's onupdate hook.
    // pickerTab tracks which of the three subsections (poster /
    // backdrop / overlay) is rendered inside the modal body.
    pickerOpen: false,
    pickerTab: 'poster',
    // pickerOptions caches the upstream-options-endpoint responses
    // keyed by tab kind ('poster' / 'fanart') so flipping tabs
    // doesn't re-fetch. Populated lazily on first open / tab switch
    // by Recording.js's loadOptions(). Reset to {} on recording-id
    // change so a stale strip doesn't leak across navigations.
    pickerOptions: {},
    pickerOptionsLoading: {},
    pickerOptionsError: {},
    // imageInfo carries a transient confirmation message (e.g.
    // "Refresh queued — page will update when complete.") rendered
    // by renderImageInfoToast. Cleared by an explicit dismiss or a
    // 5s setTimeout fired alongside the message.
    imageInfo: null,
  },
  // show is the /shows/:id detail page view-model. detail holds the
  // ShowDetailResponse payload (lower-case JSON keys per the
  // server's struct tags). sortKey/sortDir drive the in-page
  // recordings table sort. imageBusy disables the poster picker
  // thumbnails while a POST is in flight; imageError carries the
  // inline failure string from the most recent picker POST.
  show: {
    id: '',
    detail: null,
    loading: true,
    error: null,
    imageBusy: false,
    imageError: null,
    sortKey: 'date',
    sortDir: 'asc',
    // pickerOpen flips true when the "Edit images" header button opens
    // the show poster picker modal. Same lifecycle pattern as the
    // recording picker — onupdate syncs <dialog>.showModal()/close().
    pickerOpen: false,
    // pickerOptions caches the /api/v1/shows/:id/poster-options
    // response. null means "not yet fetched"; an empty array means
    // "fetched and upstream had nothing". Reset on show-id change so
    // a stale strip doesn't leak across navigations.
    pickerOptions: null,
    pickerOptionsLoading: false,
    pickerOptionsError: null,
    // imageInfo carries a transient confirmation message rendered as
    // an alert-info toast (mirrors recording.imageInfo).
    imageInfo: null,
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
  // offset / total / limit drive the shared Pagination component;
  // the kind filter is applied server-side now so re-tab triggers a
  // refetch with offset=0.
  history: {
    items: [],
    kind: '',             // '' = All; otherwise lowercase HistoryKind* token.
    recordingID: null,
    loading: true,
    error: null,
    offset: 0,
    total: 0,
    limit: 50,
  },
  // people is the /people index page state. q is the live search
  // query persisted to ?q=; sortKey/sortDir mirror Library's URL-
  // backed sort. items caches /api/v1/people responses across
  // navigations away and back. offset / total / limit drive the
  // shared Pagination component; search filters the current page
  // client-side (instant feedback) so changing q does NOT reset
  // offset or refetch.
  people: {
    items: [],
    q: '',
    sortKey: 'name',
    sortDir: 'asc',
    loading: true,
    error: null,
    offset: 0,
    total: 0,
    limit: 50,
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
