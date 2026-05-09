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
import state from '../state.js';
import { smartDate } from '../utils/format.js';
import ImagePickerModal, {
  renderEditImagesButton,
  renderImageErrorToast,
} from './ImagePickerModal.js';

// STATUS_META mirrors Library.js / Recording.js so badges stay
// consistent across the SPA.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// SORT_COLUMNS lists the recordings-table headers in render order.
// Matches the Library/Person sortable-column pattern.
const SORT_COLUMNS = [
  { key: 'status', label: 'Status',
    compare: (a, b) => cmpStr(a.status, b.status) },
  { key: 'date',   label: 'Date',
    compare: (a, b) => cmpStr(a.date_full, b.date_full) },
  { key: 'tour',   label: 'Tour',
    compare: (a, b) => cmpStr(a.tour, b.tour) },
  { key: 'master', label: 'Master',
    compare: (a, b) => cmpStr(a.master, b.master) },
];

const DEFAULT_SORT = { key: 'date', dir: 'asc' };

function cmpStr(a, b) {
  const sa = a == null ? '' : String(a).toLowerCase();
  const sb = b == null ? '' : String(b).toLowerCase();
  if (sa < sb) return -1;
  if (sa > sb) return 1;
  return 0;
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
    } catch (_) { /* not JSON */ }
    if (typeof err.body === 'string' && err.body.length < 240) return err.body;
  }
  return err.message || String(err);
}

// loadShow fetches /api/v1/shows/:id and parks the response on
// state.show. Used both on initial mount and after a successful
// poster pick so the highlighted thumb updates.
function loadShow(id) {
  state.show.loading = true;
  state.show.error = null;
  state.show.id = id;
  state.show.imageError = null;
  return api.get('/shows/' + encodeURIComponent(id))
    .then((body) => {
      state.show.detail = body || null;
      state.show.loading = false;
    })
    .catch((err) => {
      state.show.detail = null;
      state.show.error = err;
      state.show.loading = false;
    });
}

// posterIndexHighlight returns the index whose thumbnail should wear
// the green selection ring. Defaults to 0 — matching
// ShowImageChoice.ResolvePoster on the server.
function posterIndexHighlight() {
  const detail = state.show.detail;
  if (detail && detail.selected_poster_index != null) {
    return detail.selected_poster_index;
  }
  return 0;
}

// pickPoster issues the POST and re-fetches on success so
// selected_poster_index reflects the new pick.
function pickPoster(index) {
  if (state.show.imageBusy) return;
  const id = state.show.id;
  state.show.imageBusy = true;
  state.show.imageError = null;
  api.post('/shows/' + encodeURIComponent(id) + '/poster', { index })
    .then((resp) => {
      state.show.imageBusy = false;
      if (resp && resp.ok) {
        return loadShow(id);
      }
      state.show.imageError = (resp && resp.error) || 'unknown error';
      m.redraw();
      return null;
    })
    .catch((err) => {
      state.show.imageBusy = false;
      if (err && err.status === 503) {
        state.show.imageError =
          'Image cache not configured — set library.imageRoot on the server.';
      } else {
        state.show.imageError = errorMessage(err);
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
  const urls = detail.local_poster_urls || [];
  const idx = posterIndexHighlight();
  const posterURL = urls[idx] || urls[0] || '';
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
            onclick: () => { state.show.pickerOpen = true; },
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

// renderImageThumb is the per-poster-thumbnail vnode. Modeled on the
// helper in Recording.js but poster-only (no aspect dial).
function renderImageThumb({ url, alt, selected, busy, onclick }) {
  const ringClass = selected ? ' ring-2 ring-success' : '';
  const opacityClass = busy ? ' opacity-50' : '';
  return m('button', {
    type: 'button',
    class: 'btn btn-ghost p-0 h-auto rounded' + ringClass + opacityClass,
    disabled: busy,
    onclick,
    'aria-pressed': selected ? 'true' : 'false',
  }, m('figure', {
    class: 'rounded overflow-hidden bg-base-200 w-32 aspect-[2/3]',
  }, m('img', {
    src: url,
    alt,
    class: 'w-full h-full object-cover',
    loading: 'lazy',
  })));
}

// renderPosterPicker lays out one thumbnail per cached poster with
// the active one highlighted by the green ring. Shows are
// poster-only — no backdrop / overlay sections like Recording.js.
// Rendered inside the picker modal opened by the header's
// "Edit images" button; image errors surface via the toast outside
// the modal so a failed POST still reaches the user mid-close.
function renderPosterPicker(detail) {
  const urls = detail.local_poster_urls || [];
  const highlight = posterIndexHighlight();
  const busy = state.show.imageBusy;
  return m('section', { class: 'space-y-2' }, [
    m('h3', { class: 'text-sm font-semibold' },
      'Posters · ' + urls.length + ' available'),
    urls.length === 0
      ? m('div', { class: 'opacity-60 text-sm' },
          'No posters cached yet — sync to fetch.')
      : m('div', { class: 'flex flex-wrap gap-3' },
          urls.map((url, i) => renderImageThumb({
            url,
            alt: 'poster ' + (i + 1),
            selected: i === highlight,
            busy,
            onclick: () => pickPoster(i),
          }))),
  ]);
}

// renderImagePickerModal mounts the <dialog>-based modal that hosts
// the poster picker. Toggled by state.show.pickerOpen via the
// "Edit images" header button. Single-section variant (no tabs) since
// shows don't get burned-in backdrops or overlay text.
function renderImagePickerModal(detail) {
  return m(ImagePickerModal, {
    open: !!state.show.pickerOpen,
    onClose: () => { state.show.pickerOpen = false; },
    title: 'Edit images',
    busy: !!state.show.imageBusy,
    render: () => renderPosterPicker(detail),
  });
}

// Row renders one in-show recording. Click navigates to the
// per-recording detail page.
function Row(it) {
  const meta = STATUS_META[it.status] || STATUS_META.orphan;
  const date = smartDate(it.date_full, it.date_month_known, it.date_day_known);
  return m('tr', {
    class: 'hover:bg-base-200 cursor-pointer',
    onclick: () => m.route.set('/recordings/' + it.id),
  }, [
    m('td', m('span', { class: 'badge ' + meta.badge }, meta.label)),
    m('td', { class: 'font-mono text-sm' }, date),
    m('td', it.tour || '—'),
    m('td', it.master || '—'),
  ]);
}

function renderRecordingsTable(detail) {
  const recordings = detail.recordings || [];
  const sorted = sortRows(recordings,
    { key: state.show.sortKey, dir: state.show.sortDir });
  return m('div', { class: 'space-y-2' }, [
    m('h2', { class: 'text-sm font-semibold uppercase tracking-wider opacity-70' },
      'Recordings · ' + recordings.length),
    m('div', { class: 'overflow-x-auto rounded-box bg-base-200' },
      m('table', { class: 'table table-zebra' }, [
        m('thead', m('tr',
          SORT_COLUMNS.map((col) => HeaderCell(col, state.show.sortKey, state.show.sortDir)))),
        m('tbody', sorted.length === 0
          ? m('tr', m('td', {
              colspan: SORT_COLUMNS.length, class: 'text-center opacity-60 py-8',
            }, 'No recordings of this show in your library yet.'))
          : sorted.map(Row)),
      ])),
  ]);
}

const Show = {
  oninit(vnode) {
    state.show.imageBusy = false;
    state.show.imageError = null;
    state.show.pickerOpen = false;
    state.show.sortKey = DEFAULT_SORT.key;
    state.show.sortDir = DEFAULT_SORT.dir;
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
      state.show.pickerOpen = false;
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
      renderRecordingsTable(detail),
      renderImagePickerModal(detail),
      renderImageErrorToast({
        error: state.show.imageError,
        onDismiss: () => { state.show.imageError = null; },
      }),
    ]);
  },
};

export default Show;
