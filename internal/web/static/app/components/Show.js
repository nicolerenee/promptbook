// Show.js — /shows/:id detail page.
//
// Per-show analog of Recording.js. Renders:
//   1. Header card — selected show poster on the left, name + meta on
//      the right (year span, recording count, state badge cluster).
//   2. Poster picker — same shape as Recording.js's picker, but
//      poster-only (shows don't get burned-in overlays, so there's no
//      backdrop or overlay-text editor). Click a thumb -> POST
//      /api/v1/shows/:id/poster -> re-fetch -> re-render.
//   3. Recordings table — full list of in-show recordings, sortable
//      by date / tour / status / master. Click a row -> /recordings/:id.
//
// Wire payload (/api/v1/shows/:id) keys are the lower-case JSON tags
// declared on ShowDetailResponse server-side; STATUS_META keys on the
// recording.status string.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import graphql from '../graphql.js';
import state from '../state.js';
import { smartDateWithVariant } from '../utils/format.js';
import {
  uploadFile,
  errorMessageFromUpload,
} from '../utils/uploadPicker.js';
import ImagePickerModal, {
  renderEditImagesButton,
  renderImageErrorToast,
  renderImageInfoToast,
  renderUpstreamPicker,
} from './ImagePickerModal.js';

// STATUS_META mirrors Library.js / Recording.js so badges stay
// consistent across the SPA.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  out_of_sync: { label: 'Out of sync', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// SORT_COLUMNS lists the recordings-table headers in render order.
// Status sits at the FAR RIGHT to match the Recordings + ShowsList
// convention — row identity (date/tour/master) belongs on the left,
// the at-a-glance status badge on the right.
const SORT_COLUMNS = [
  { key: 'date',   label: 'Date',
    compare: (a, b) => cmpStr(a.date_full, b.date_full) },
  { key: 'tour',   label: 'Tour',
    compare: (a, b) => cmpStr(a.tour, b.tour) },
  { key: 'master', label: 'Master',
    compare: (a, b) => cmpStr(a.master, b.master) },
  { key: 'status', label: 'Status',
    compare: (a, b) => cmpStr(a.status, b.status) },
];

const DEFAULT_SORT = { key: 'date', dir: 'asc' };

// LS_VIEW persists the user's preferred recordings layout (list vs
// grid) on the show-detail page. Mirrors the per-page-pref pattern
// from Recordings.js + ShowsList.js. A separate key from those pages
// since the show-detail recording grid is a different surface.
const LS_RECORDINGS_VIEW = 'pb.show.recordings.view';

function readStoredRecordingsView() {
  try {
    const v = window.localStorage.getItem(LS_RECORDINGS_VIEW);
    return v === 'grid' || v === 'list' ? v : '';
  } catch (_) {
    return '';
  }
}
function writeStoredRecordingsView(v) {
  try {
    window.localStorage.setItem(LS_RECORDINGS_VIEW, v);
  } catch (_) { /* no-op */ }
}

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
}

// SHOW_DETAIL_QUERY pulls the show + its in-library recordings via
// GraphQL. The Recording subselection inside the Relay connection
// produces the same per-recording row shape the legacy ShowDetail
// REST handler emitted; mapShowDetail collapses it back to the
// snake_case keys the renderer was written against. Image-picker
// writes (banner-from-url / banner-upload / refresh-images) stay on
// REST.
const SHOW_DETAIL_QUERY = `
  query ShowDetail($id: ID!) {
    show(id: $id) {
      id
      name
      description
      recordingCount
      firstYear
      lastYear
      localBannerURL
      stateCounts {
        synced
        outOfSync
        missing
        wanted
        orphan
      }
      recordings(first: 500, orderBy: { field: DATE_FULL, direction: ASC }) {
        edges {
          node {
            id
            tour
            master
            dateFull
            dateMonthKnown
            dateDayKnown
            dateVariant
            status
            inCollection
            inWants
            encoraFormat
            localFormatString
            localPosterURL
          }
        }
      }
    }
  }
`;

// stripIDPrefix turns "show-1234" into "1234".
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// mapShowDetail rewrites a GraphQL Show node into the snake_case
// shape the renderer expects. state_counts is a flat
// {[token]: count} map post-mapping. Recordings come out as a flat
// array (the renderer doesn't distinguish edges).
function mapShowDetail(node) {
  if (!node) return null;
  const sc = node.stateCounts || {};
  const recs = ((node.recordings && node.recordings.edges) || [])
    .map((e) => e && e.node)
    .filter(Boolean)
    .map((r) => ({
      id:               Number(stripIDPrefix(r.id)),
      tour:             r.tour || '',
      master:           r.master || '',
      date_full:        r.dateFull || '',
      date_month_known: !!r.dateMonthKnown,
      date_day_known:   !!r.dateDayKnown,
      date_variant:     r.dateVariant || '',
      status:           r.status || '',
      in_collection:    !!r.inCollection,
      in_wants:         !!r.inWants,
      encora_format:    r.encoraFormat || '',
      local_format:     r.localFormatString || '',
      local_poster_url: r.localPosterURL || '',
    }));
  return {
    id:               Number(stripIDPrefix(node.id)),
    name:             node.name || '',
    description:      node.description || '',
    recording_count:  node.recordingCount || 0,
    first_year:       node.firstYear == null ? null : node.firstYear,
    last_year:        node.lastYear == null ? null : node.lastYear,
    local_banner_url: node.localBannerURL || '',
    state_counts: {
      synced:          sc.synced || 0,
      out_of_sync: sc.outOfSync || 0,
      missing:         sc.missing || 0,
      wanted:          sc.wanted || 0,
      orphan:          sc.orphan || 0,
    },
    recordings: recs,
  };
}

// loadShow fetches the show detail payload via GraphQL and parks the
// shaped result on state.show. Used both on initial mount and after a
// successful poster pick so the highlighted thumb updates.
function loadShow(id) {
  state.show.loading = true;
  state.show.error = null;
  state.show.id = id;
  state.show.imageError = null;
  return graphql.query(SHOW_DETAIL_QUERY, { id: 'show-' + id })
    .then((data) => {
      const node = data && data.show;
      if (!node) {
        const err = new Error('Show not found.');
        err.status = 404;
        throw err;
      }
      state.show.detail = mapShowDetail(node);
      state.show.loading = false;
    })
    .catch((err) => {
      state.show.detail = null;
      state.show.error = err;
      state.show.loading = false;
    });
}

// runBannerUpload uploads a chosen file to the show's banner slot and
// re-loads the detail so local_banner_url picks up the new image.
// Uploads commit immediately because the file picker IS the explicit
// confirmation gesture; the staged-then-Save flow only applies to
// upstream URL picks (which are easier to mis-click).
function runBannerUpload(file) {
  if (!file) return;
  if (state.show.imageBusy) return;
  const id = state.show.id;
  state.show.imageBusy = true;
  state.show.imageError = null;
  m.redraw();

  uploadFile('/api/v1/shows/' + encodeURIComponent(id) + '/banner-upload', file)
    .then((resp) => {
      state.show.imageBusy = false;
      if (!resp || resp.ok !== true) {
        state.show.imageError = (resp && resp.error) || 'upload failed';
        m.redraw();
        return null;
      }
      // Bump the version counter so every <img src> downstream gets a
      // fresh ?v=… and the browser refetches instead of serving the
      // pre-upload bytes from cache.
      state.show.imageVersion = (state.show.imageVersion || 0) + 1;
      return loadShow(id);
    })
    .catch((err) => {
      state.show.imageBusy = false;
      if (err && err.status === 503) {
        state.show.imageError =
          'Image cache not configured — set library.imageRoot on the server.';
      } else if (err && err.status === 413) {
        state.show.imageError =
          'Upload too large — keep the file under 10 MB.';
      } else {
        state.show.imageError = errorMessageFromUpload(err);
      }
      m.redraw();
    });
}

// HeaderCell renders one sortable <th> following the same affordances
// as Library.js / Person.js.
function HeaderCell(col, sortKey, sortDir) {
  const isActive = col.key === sortKey;
  let caret = '';
  if (isActive) caret = sortDir === 'desc' ? ' ▼' : ' ▲';
  const ariaSort = isActive
    ? (sortDir === 'desc' ? 'descending' : 'ascending')
    : 'none';
  return m('th', {
    class: 'cursor-pointer select-none',
    'aria-sort': ariaSort,
    tabindex: 0,
    role: 'button',
    onclick: () => setSort(col.key),
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') {
        ev.preventDefault();
        setSort(col.key);
      }
    },
  }, col.label + caret);
}

function setSort(key) {
  const s = state.show;
  if (s.sortKey === key) {
    s.sortDir = s.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    s.sortKey = key;
    s.sortDir = key === 'date' ? 'desc' : 'asc';
  }
}

function sortRows(rows, sort) {
  const col = SORT_COLUMNS.find((c) => c.key === sort.key);
  if (!col) return rows.slice();
  const sign = sort.dir === 'desc' ? -1 : 1;
  const out = rows.slice();
  out.sort((a, b) => sign * col.compare(a, b));
  return out;
}

// renderHeader is the show header card: selected poster on the left
// (or a placeholder), name + meta on the right, and a small
// right-aligned "Edit images" action that opens the poster picker
// modal.
function renderHeader(detail) {
  const posterURL = withImageVersion(detail.local_banner_url || '');
  const span = yearSpanText(detail);
  const stateCounts = detail.state_counts || {};
  const badges = Object.keys(stateCounts)
    .sort()
    .filter((k) => stateCounts[k] > 0)
    .map((k) => {
      const meta = STATUS_META[k] || STATUS_META.orphan;
      return m('span', { class: 'badge ' + meta.badge },
        meta.label + ' · ' + stateCounts[k]);
    });
  return m('div', { class: 'card lg:card-side bg-base-100 shadow-sm overflow-hidden' }, [
    posterURL
      ? m('figure', { class: 'lg:w-64 shrink-0' }, m('img', {
          src: posterURL,
          alt: (detail.name || 'show') + ' poster',
          class: 'w-full h-full object-cover',
          loading: 'lazy',
        }))
      : m('figure', {
          class: 'lg:w-64 shrink-0 aspect-[2/3] bg-base-200 ' +
                 'flex items-center justify-center text-base-content/40 ' +
                 'text-sm font-mono',
        }, 'no poster'),
    m('div', { class: 'card-body' }, [
      m('div', { class: 'flex items-start justify-between gap-3 flex-wrap' }, [
        m('div', { class: 'min-w-0' }, [
          m('div', {
            class: 'text-xs uppercase tracking-wider opacity-60',
          }, 'Show · s-' + detail.id),
          m('h1', { class: 'text-3xl font-semibold' }, detail.name || '—'),
          m('p', { class: 'text-sm opacity-70 font-mono' },
            (span ? span + ' · ' : '') +
            detail.recording_count + ' recording' +
            (detail.recording_count === 1 ? '' : 's')),
        ]),
        m('div', { class: 'shrink-0' },
          renderEditImagesButton({
            onclick: () => {
              state.show.pickerOpen = true;
              // Kick the upstream options fetch on first open so the
              // picker isn't a static skeleton until the user clicks
              // around. Cached options stay between opens.
              if (!Array.isArray(state.show.pickerOptions)) {
                loadShowOptions(detail.id);
              }
            },
          })),
      ]),
      detail.description
        ? m('p', { class: 'text-sm opacity-80 max-w-2xl whitespace-pre-line' },
            detail.description)
        : null,
      badges.length > 0
        ? m('div', { class: 'flex flex-wrap gap-2 mt-2' }, badges)
        : null,
    ]),
  ]);
}

// yearSpanText formats first_year / last_year into "2017 — 2024",
// or just "2017" when both equal, or "" when neither is present.
function yearSpanText(detail) {
  const first = detail.first_year;
  const last = detail.last_year;
  if (first == null && last == null) return '';
  if (first != null && last != null) {
    if (first === last) return String(first);
    return first + ' — ' + last;
  }
  return String(first != null ? first : last);
}

// errorMessage extracts a human-readable string from a thrown api
// error. Mirrors the helper in Recording.js.
function errorMessage(err) {
  if (!err) return 'unknown error';
  if (err.body) {
    try {
      const parsed = JSON.parse(err.body);
      if (parsed && parsed.error) return String(parsed.error);
      if (parsed && parsed.message) return String(parsed.message);
    } catch (_) { /* not JSON; fall through. */ }
    if (typeof err.body === 'string' && err.body.length < 240) return err.body;
  }
  return err.message || String(err);
}

// loadShowOptions fetches /api/v1/shows/:id/poster-options and
// parks the result on state.show.pickerOptions. Errors land on
// pickerOptionsError so the picker tab can render an inline alert.
function loadShowOptions(id) {
  state.show.pickerOptionsLoading = true;
  state.show.pickerOptionsError = null;
  m.redraw();
  return api.get('/shows/' + encodeURIComponent(id) + '/poster-options')
    .then((body) => {
      state.show.pickerOptions =
        (body && Array.isArray(body.options)) ? body.options : [];
      // Bump the generation so the picker forces fresh <img> requests
      // (cache-buster + Mithril key churn). See the matching comment
      // in Recording.js loadOptions for the full rationale.
      state.show.pickerOptionsGen = (state.show.pickerOptionsGen || 0) + 1;
      state.show.pickerOptionsLoading = false;
      m.redraw();
    })
    .catch((err) => {
      state.show.pickerOptionsLoading = false;
      if (err && err.status === 503) {
        state.show.pickerOptionsError =
          'Upstream client not configured on the server.';
      } else {
        state.show.pickerOptionsError = errorMessage(err);
      }
      m.redraw();
    });
}

// stagePickerSelection records the URL the user clicked. Click-to-stage
// (rather than click-to-submit) means a misclick on a thumbnail is
// reversible — the user clicks Save in the footer to actually commit.
function stagePickerSelection(url) {
  state.show.pickerStaged = url || null;
}

// commitPickerSelection POSTs /shows/:id/banner-from-url with the
// currently staged URL; on success bumps imageVersion + re-fetches the
// detail so local_banner_url picks up the new slot AND the cache-bust
// suffix forces the browser to refetch the image bytes (the path is
// stable, so without ?v= the same <img src> would render the cached
// pre-pick image).
function commitPickerSelection(id) {
  const url = state.show.pickerStaged;
  if (!url) return;
  if (state.show.imageBusy) return;
  state.show.imageBusy = true;
  state.show.imageError = null;
  m.redraw();
  api.post('/shows/' + encodeURIComponent(id) + '/banner-from-url', { url })
    .then((resp) => {
      state.show.imageBusy = false;
      if (resp && resp.ok) {
        state.show.imageVersion = (state.show.imageVersion || 0) + 1;
        state.show.pickerStaged = null;
        return loadShow(id);
      }
      state.show.imageError = (resp && resp.error) || 'unknown error';
      m.redraw();
      return null;
    })
    .catch((err) => {
      state.show.imageBusy = false;
      state.show.imageError = errorMessage(err);
      m.redraw();
    });
}

// runShowRefreshFromUpstream fires the per-entity refresh-show-images
// job with force=true, then closes the modal and parks an info-toast
// message so the user knows the work is queued.
function runShowRefreshFromUpstream(id) {
  state.show.imageBusy = true;
  m.redraw();
  api.post('/shows/' + encodeURIComponent(id) + '/refresh-images',
    { force: true })
    .then(() => {
      state.show.imageBusy = false;
      state.show.pickerOpen = false;
      state.show.imageInfo =
        'Refresh queued — page will update when the job completes.';
      setTimeout(() => {
        if (state.show.imageInfo) {
          state.show.imageInfo = null;
          m.redraw();
        }
      }, 5000);
      m.redraw();
    })
    .catch((err) => {
      state.show.imageBusy = false;
      if (err && err.status === 503) {
        state.show.imageError =
          'Background jobs not configured on the server.';
      } else if (err && err.status === 409) {
        state.show.imageError =
          'A refresh is already in flight — wait for it to finish.';
      } else {
        state.show.imageError = errorMessage(err);
      }
      m.redraw();
    });
}

// renderPosterPicker is the show-banner tab body. Single slot under
// v2 — clicking an upstream thumbnail STAGES it (a primary ring marks
// the selection); the modal footer's "Save" button commits the staged
// URL via /shows/:id/banner-from-url.
function renderPosterPicker(detail) {
  const id = detail.id;
  return renderUpstreamPicker({
    currentURL: withImageVersion(detail.local_banner_url || ''),
    currentLabel: 'Current banner',
    currentAlt: (detail.name || 'show') + ' banner',
    aspect: 'poster',
    options: state.show.pickerOptions,
    loading: !!state.show.pickerOptionsLoading,
    error: state.show.pickerOptionsError || null,
    busy: !!state.show.imageBusy,
    loadGen: state.show.pickerOptionsGen || 0,
    staged: state.show.pickerStaged || null,
    onPick: stagePickerSelection,
    onUpload: runBannerUpload,
    onRefetch: () => loadShowOptions(id),
    uploadLabel: 'Upload banner',
  });
}

// withImageVersion appends the current state.show.imageVersion as
// ?v=<n> to a /images/... URL so a save-and-refresh re-fetches the
// bytes. Without this the browser sees the same canonical path and
// serves the pre-save image from cache.
function withImageVersion(url) {
  if (!url) return url;
  const v = state.show.imageVersion || 0;
  if (!v) return url;
  return url + (url.indexOf('?') >= 0 ? '&' : '?') + 'v=' + v;
}

// renderImagePickerModal mounts the <dialog>-based modal that hosts
// the poster picker. Toggled by state.show.pickerOpen via the
// "Edit images" header button. Single-section variant (no tabs) since
// shows don't get burned-in backdrops or overlay text.
function renderImagePickerModal(detail) {
  const staged = state.show.pickerStaged || null;
  const busy = !!state.show.imageBusy;
  const footerActions = [
    {
      label: 'Save selection',
      primary: true,
      disabled: !staged || busy,
      onClick: () => commitPickerSelection(detail.id),
    },
    {
      label: 'Refresh from upstream',
      primary: false,
      disabled: busy,
      onClick: () => runShowRefreshFromUpstream(detail.id),
    },
  ];
  return m(ImagePickerModal, {
    open: !!state.show.pickerOpen,
    onClose: () => {
      state.show.pickerOpen = false;
      // Drop the staged URL on close so re-opening the modal doesn't
      // resurrect a half-applied selection from a prior session.
      state.show.pickerStaged = null;
    },
    title: 'Edit images',
    busy,
    render: () => renderPosterPicker(detail),
    footerActions,
  });
}

// Row renders one in-show recording. Click navigates to the
// per-recording detail page.
// Row renders one recording. Column order matches SORT_COLUMNS so
// Date → Tour → Master → Status, with the status badge on the right
// edge to match Recordings + ShowsList.
function Row(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const date = smartDateWithVariant(
    it.date_full, it.date_month_known, it.date_day_known, it.date_variant);
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + it.id),
  }, [
    m('td', { class: 'font-mono text-sm' }, date),
    m('td', it.tour || '—'),
    m('td', it.master || '—'),
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
  ]);
}

// PosterCard renders one recording-poster cell for the grid view.
// Mirrors Recordings.js's PosterCard so the visual rhythm reads as
// the same component in two places. Status badge floats outside the
// image-clip via DaisyUI's indicator pattern.
function PosterCard(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const poster = it.local_poster_url || '';
  const onclick = () => m.route.set('/recordings/' + it.id);
  const placeholder = m('div', {
    class: 'aspect-[2/3] w-full bg-base-300 flex items-center ' +
           'justify-center text-xs opacity-60 px-2 text-center',
  }, m('span', { class: 'badge ' + meta.badge }, meta.label));
  const image = m('img', {
    src: poster,
    alt: (it.tour || it.master || '') + ' poster',
    loading: 'lazy',
    class: 'aspect-[2/3] w-full object-cover',
  });
  const date = smartDateWithVariant(
    it.date_full, it.date_month_known, it.date_day_known, it.date_variant);
  return m('div', {
    class: 'card bg-base-200 shadow-sm hover:shadow-md ' +
           'hover:ring-1 hover:ring-primary cursor-pointer transition-shadow',
    onclick,
    role: 'button',
    tabindex: 0,
    onkeydown: (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); onclick(); }
    },
  }, [
    m('div', { class: 'indicator w-full' }, [
      m('span', { class: 'indicator-item badge badge-sm ' + meta.badge },
        meta.label),
      m('div', { class: 'overflow-hidden rounded-t-box w-full' },
        poster ? image : placeholder),
    ]),
    m('div', { class: 'card-body p-2 gap-0.5' }, [
      m('div', { class: 'text-sm font-medium truncate', title: it.tour || '' },
        it.tour || '—'),
      m('div', { class: 'text-xs opacity-60 font-mono truncate' },
        date + (it.master ? ' · ' + it.master : '')),
    ]),
  ]);
}

// IconList / IconGrid + ViewToggle mirror the Recordings.js helpers
// so the segmented-button reads as the same control across pages.
function IconList() {
  return m('svg', {
    width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none',
    stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', 'aria-hidden': 'true',
  }, [
    m('line', { x1: 8, y1: 6, x2: 21, y2: 6 }),
    m('line', { x1: 8, y1: 12, x2: 21, y2: 12 }),
    m('line', { x1: 8, y1: 18, x2: 21, y2: 18 }),
    m('line', { x1: 3, y1: 6, x2: 3.01, y2: 6 }),
    m('line', { x1: 3, y1: 12, x2: 3.01, y2: 12 }),
    m('line', { x1: 3, y1: 18, x2: 3.01, y2: 18 }),
  ]);
}
function IconGrid() {
  return m('svg', {
    width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none',
    stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', 'aria-hidden': 'true',
  }, [
    m('rect', { x: 3, y: 3, width: 7, height: 7 }),
    m('rect', { x: 14, y: 3, width: 7, height: 7 }),
    m('rect', { x: 3, y: 14, width: 7, height: 7 }),
    m('rect', { x: 14, y: 14, width: 7, height: 7 }),
  ]);
}

function setRecordingsView(key) {
  if (key !== 'list' && key !== 'grid') return;
  state.show.recordingsView = key;
  writeStoredRecordingsView(key);
}

function ViewToggle(view) {
  return m('div', { class: 'join', role: 'group', 'aria-label': 'View mode' }, [
    m('button', {
      type: 'button',
      class: 'btn btn-sm join-item' + (view === 'list' ? ' btn-active' : ''),
      'aria-pressed': view === 'list',
      title: 'List view',
      onclick: () => setRecordingsView('list'),
    }, IconList()),
    m('button', {
      type: 'button',
      class: 'btn btn-sm join-item' + (view === 'grid' ? ' btn-active' : ''),
      'aria-pressed': view === 'grid',
      title: 'Grid view',
      onclick: () => setRecordingsView('grid'),
    }, IconGrid()),
  ]);
}

function renderRecordingsList(sorted) {
  return m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
    m('table', { class: 'table table-zebra' }, [
      m('thead', m('tr',
        SORT_COLUMNS.map((col) => HeaderCell(col, state.show.sortKey, state.show.sortDir)))),
      m('tbody', sorted.length === 0
        ? m('tr', m('td', {
            colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
          }, 'No recordings of this show in your library yet.'))
        : sorted.map(Row)),
    ]));
}

function renderRecordingsGrid(sorted) {
  if (sorted.length === 0) {
    return m('div', { class: 'text-center opacity-60 py-12' },
      'No recordings of this show in your library yet.');
  }
  return m('div', {
    class: 'grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 ' +
           'lg:grid-cols-5 xl:grid-cols-6 gap-4',
  }, sorted.map(PosterCard));
}

function renderRecordingsSection(detail) {
  const recordings = detail.recordings || [];
  const sorted = sortRows(recordings,
    { key: state.show.sortKey, dir: state.show.sortDir });
  const view = state.show.recordingsView || 'list';
  // Mirrors the wrapper Recording.js uses for its Files / Cast
  // sections so the two pages read the same — bordered card with a
  // card-title heading instead of the bare uppercase eyebrow.
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('div', { class: 'flex items-center justify-between gap-3 flex-wrap' }, [
        m('h2', { class: 'card-title text-base' },
          'Recordings · ' + recordings.length),
        ViewToggle(view),
      ]),
      view === 'grid'
        ? renderRecordingsGrid(sorted)
        : renderRecordingsList(sorted),
    ]));
}

const Show = {
  oninit(vnode) {
    state.show.imageBusy = false;
    state.show.imageError = null;
    state.show.imageInfo = null;
    state.show.pickerOpen = false;
    state.show.pickerOptions = null;
    state.show.pickerOptionsLoading = false;
    state.show.pickerOptionsError = null;
    state.show.sortKey = DEFAULT_SORT.key;
    state.show.sortDir = DEFAULT_SORT.dir;
    state.show.recordingsView = readStoredRecordingsView() || 'list';
    const id = vnode.attrs && vnode.attrs.id;
    if (!id) {
      state.show.loading = false;
      state.show.error = new Error('Missing show id.');
      return;
    }
    loadShow(id);
  },

  onupdate(vnode) {
    const id = vnode.attrs && vnode.attrs.id;
    if (id && state.show.id !== id) {
      state.show.imageBusy = false;
      state.show.imageError = null;
      state.show.imageInfo = null;
      state.show.pickerOpen = false;
      state.show.pickerOptions = null;
      state.show.pickerOptionsLoading = false;
      state.show.pickerOptionsError = null;
      loadShow(id);
    }
  },

  view() {
    const s = state.show;
    if (s.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading show…');
    }
    if (s.error) {
      const err = s.error;
      if (err && err.status === 404) {
        return m('div', { role: 'alert', class: 'alert alert-warning' }, [
          m('span', 'Show not found.'),
        ]);
      }
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load show: ' +
          (err && err.message ? err.message : String(err))),
      ]);
    }
    const detail = s.detail;
    if (!detail) {
      return m('div', { class: 'p-8 opacity-60' }, 'No show data.');
    }
    return m('div', { class: 'space-y-6' }, [
      renderHeader(detail),
      renderRecordingsSection(detail),
      renderImagePickerModal(detail),
      renderImageErrorToast({
        error: state.show.imageError,
        onDismiss: () => { state.show.imageError = null; },
      }),
      renderImageInfoToast({
        message: state.show.imageInfo,
        onDismiss: () => { state.show.imageInfo = null; },
      }),
    ]);
  },
};

export default Show;
