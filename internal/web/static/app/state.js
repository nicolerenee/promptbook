// state.js — exported reactive state objects shared across components.
//
// Mithril doesn't ship a store; mutations on plain objects show up on
// the next m.redraw(). Each top-level key here owns one page's
// view-model so unrelated pages don't trip over each other. Mutators
// stay on the components that own them — this file only declares the
// shape + initial values.

export const state = {
  // recordings is the /recordings (a.k.a. /) list page view-model.
  // Status filter chips drive `status`; sort + view + pagination work
  // the same as before. Wants are reachable via status='wanted'; the
  // separate Wants page is gone.
  recordings: {
    items: [],
    status: '',           // '' = All; otherwise lowercase storage.Status.
    sortKey: 'recording',
    sortDir: 'asc',
    loading: true,
    error: null,
    view: 'list',         // 'list' | 'grid'.
    offset: 0,
    total: 0,
    limit: 50,
  },
  // showsList is the /shows list page view-model. Same shape as
  // recordings minus status filter (by-show aggregates are
  // multi-status by definition). `showsList` (not `shows`) so it
  // doesn't collide with `state.show` (the show DETAIL page).
  showsList: {
    items: [],
    sortKey: 'name',
    sortDir: 'asc',
    loading: true,
    error: null,
    view: 'list',         // 'list' | 'grid'.
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
    // Rename / NFO regen operator surfaces. renameOpen drives the
    // <dialog>-based rename preview/apply modal; renamePreview holds
    // the per-version preview rows (null while loading or unfetched);
    // renameApplying is true while the apply mutation is in flight;
    // renameResult holds the per-version outcomes after apply lands so
    // the modal can switch from the preview view to the result view.
    // regeneratingNFO disables the "Regenerate NFO" button while the
    // mutation flies; regenerateNFOMessage carries the inline
    // confirmation / error text the toast renders alongside the
    // existing image toasts.
    renameOpen: false,
    renamePreview: null,
    renamePreviewLoading: false,
    renamePreviewError: null,
    renameApplying: false,
    renameResult: null,
    renameApplyError: null,
    regeneratingNFO: false,
    regenerateNFOMessage: null,
    regenerateNFOError: null,
    // expandedVersions tracks which Files-section Versions table rows
    // are showing their inline Media Info expansion. Keyed by the
    // row's index in the versions array. Reset on every recording
    // navigation so a previously-open row doesn't bleed onto a
    // different recording.
    expandedVersions: {},
    // nfoExpanded toggles the bottom-of-Files-section NFO disclosure.
    // Collapsed by default — the user opts in to read the XML.
    nfoExpanded: false,
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
  // queue mirrors the manual_import_queue view-model. items holds
  // the latest GraphQL `queue` payload (with the rich
  // suggestedRecording field unwrapped per row). importingItem +
  // importingLocal drive the QueueImportModal: importingItem is the
  // row whose modal is open (null when closed); importingLocal is
  // the per-modal local state (typed query, search results, picked
  // match, preview path, in-flight flag) — held on the page so it
  // survives Mithril redraws while the modal is open.
  queue: {
    items: [],
    loading: true,
    error: null,
    importingItem:  null,
    importingLocal: null,
    // rescanning is true while the Re-scan button has fired the
    // scan-incoming job and is waiting for the refetch. rescanError
    // carries a short inline message when the trigger fails (jobs
    // not configured, scan already running, etc.).
    rescanning:  false,
    rescanError: null,
    // scanningLibrary mirrors rescanning but for the Scan library
    // button — fires the scan-library-root job (manual-only, no
    // recurring schedule) so orphan recordings already living in
    // library.root get backfilled into the queue. scanLibraryError
    // carries the inline message when the trigger itself fails.
    scanningLibrary:  false,
    scanLibraryError: null,
    // successToast is set by the queue page after a successful
    // import so the view renders a "Imported · {show}" toast with a
    // "View recording" link. Cleared by the dismiss button, the
    // 8-second auto-dismiss timer, or by clicking through to the
    // recording. Object shape: { recordingID, show, duplicate,
    // timerScheduled }.
    successToast: null,
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
