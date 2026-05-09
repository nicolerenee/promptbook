// Recording.js — Mithril port of the legacy /static/recording.js.
//
// Renders the /recordings/:id detail page. The wire payload
// (/api/v1/recordings/:id) embeds *storage.LoadedRecording, whose Go
// fields carry no json tags — so PascalCase keys land verbatim in the
// JSON. The server-side enrichment fields are the lower-case ones the
// recordingDetailResponse wrapper adds (posters, nfo_content,
// nfo_modified_at).
//
// Page anatomy (top to bottom):
//   1. Header — show title, "enc-NNNN" muted, status + NFT badges
//   2. Two-column body
//      Left column:  poster card, metadata card, NFT callout, NFO card
//      Right column: cast card, versions card
//   3. Danger zone — divider above, single state-driven button gated
//      by a typed-confirmation prompt (window.prompt, matching legacy)
//
// Status taxonomy (lowercase, identical to Library.js):
//   synced / format_mismatch / missing / wanted / orphan
// Status is derived client-side via statusForRecording(), mirroring the
// legacy helper which mirrors storage.ResolveStatus.
//
// NFT callout copy is verbatim from the legacy recording.js — the user
// explicitly corrected these strings earlier. Keep the wording.
//
// Image picker UI for posters/backdrops is OUT of scope for this port;
// we render the first poster only and fall back to a placeholder.

import m from 'https://esm.sh/mithril@2.2.2';
import api from '../api.js';
import state from '../state.js';
import { smartDate, humanSize, relativeTime, formatNFTDate } from '../utils/format.js';
import {
  uploadFile,
  errorMessageFromUpload,
  renderUploadButton,
} from '../utils/uploadPicker.js';
import ImagePickerModal, {
  renderEditImagesButton,
  renderImageErrorToast,
} from './ImagePickerModal.js';

// STATUS_META keys on the lowercase status tokens, matching Library.js.
// Same DaisyUI badge color modifiers so the visual language stays
// consistent across pages.
const STATUS_META = {
  synced:          { label: 'Synced',          badge: 'badge-success' },
  format_mismatch: { label: 'Format mismatch', badge: 'badge-warning' },
  missing:         { label: 'Missing',         badge: 'badge-error' },
  wanted:          { label: 'Wanted',          badge: 'badge-info' },
  orphan:          { label: 'Orphan',          badge: 'badge-neutral' },
};

// statusForRecording mirrors storage.ResolveStatus on the data we have.
// Returns the lowercase API token used by STATUS_META for badge lookup.
function statusForRecording(loaded) {
  const versions = (loaded && loaded.Versions) || [];
  const hasFile = versions.length > 0;
  if (loaded.InCollection) {
    if (!hasFile) return 'missing';
    const local = loaded.LocalFormatString || '';
    const encora = loaded.Format || '';
    if (local && encora && local !== encora) return 'format_mismatch';
    return 'synced';
  }
  if (loaded.InWants) return 'wanted';
  return 'orphan';
}

// dangerActionFor picks the single destructive action that makes
// sense for the recording's current Encora state, mirroring the legacy
// helper. Returns null when no action applies (orphan recordings still
// get the constructive "Add to wants" branch — there is no null case
// in practice).
function dangerActionFor(loaded) {
  const id = loaded.Recording.id;
  if (loaded.InCollection) {
    return {
      kind:        'remove_collection',
      label:       'Remove from collection',
      path:        '/encora/collection/' + id + '/remove',
      description: 'This removes the recording from your Encora ' +
                   'collection. Local files stay on disk; only the ' +
                   'upstream catalog row goes.',
      destructive: true,
    };
  }
  if (loaded.InWants) {
    return {
      kind:        'remove_wants',
      label:       'Remove from wants',
      path:        '/encora/wants/' + id + '/remove',
      description: 'This removes the recording from your Encora ' +
                   'wants list.',
      destructive: true,
    };
  }
  return {
    kind:        'add_wants',
    label:       'Add to wants',
    path:        '/encora/wants/' + id + '/add',
    description: 'This adds the recording to your Encora wants list.',
    destructive: false,
  };
}

// errorMessage extracts a human-readable string from a thrown api
// error. Mirrors the helper in legacy recording.js so 409 responses
// surface their {error: "..."} payload, with a fallback chain back to
// err.message. Used by the danger-zone POST handler.
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

// dirname / basename are shared by the version table — pulled out as
// tiny helpers rather than imported because they only matter here and
// the legacy file inlined them too.
function dirname(p) {
  if (!p) return '';
  const i = p.lastIndexOf('/');
  if (i < 0) return p;
  return p.substring(0, i + 1);
}
function basename(p) {
  if (!p) return '';
  const i = p.lastIndexOf('/');
  if (i < 0) return p;
  return p.substring(i + 1);
}

// loadRecording fetches /api/v1/recordings/:id and parks the response
// on state.recording. Under the v2 image cache the payload exposes
// scalar image URLs (local_fanart_url, local_poster_url) rather than
// indexed arrays — the chosen image IS the only image on disk.
function loadRecording(id) {
  state.recording.loading = true;
  state.recording.error = null;
  state.recording.id = id;
  state.recording.imageError = null;
  return api.get('/recordings/' + encodeURIComponent(id))
    .then((body) => {
      state.recording.loaded = body;
      state.recording.loading = false;
      state.recording.overlayOverride =
        body && body.overlay_text_override != null
          ? body.overlay_text_override : null;
      state.recording.overlayDraft =
        state.recording.overlayOverride != null
          ? state.recording.overlayOverride
          : autoOverlayText(body);
      state.recording.overlayDisabled =
        !!(body && body.overlay_disabled);
    })
    .catch((err) => {
      state.recording.loaded = null;
      state.recording.error = err;
      state.recording.loading = false;
    });
}

// autoOverlayText derives the fallback "show · tour · date" string the
// renderer burns in when no override is set. Mirrors what the
// imagerender package will compute server-side once the stub is
// filled in — keeping it client-side too means the overlay-text
// input shows the user the exact string that would render today
// without first round-tripping through the server.
function autoOverlayText(loaded) {
  if (!loaded || !loaded.Recording) return '';
  const r = loaded.Recording;
  const parts = [];
  if (r.show) parts.push(r.show);
  if (r.tour) parts.push(r.tour);
  const date = smartDate(
    r.date && r.date.full_date,
    r.date && r.date.month_known,
    r.date && r.date.day_known,
  );
  if (date && date !== '—') parts.push(date);
  return parts.join(' · ');
}

// postPickerChoice issues a POST against the supplied path with the
// JSON body. Locks state.recording.imageBusy for the duration so the
// thumbnail buttons disable to prevent double-clicks; updates
// imageError on failure. onSuccess fires after the round-trip lands
// so each subsection can patch its own selectedX field.
function postPickerChoice(path, body, onSuccess) {
  if (state.recording.imageBusy) return;
  state.recording.imageBusy = true;
  state.recording.imageError = null;
  api.post(path, body)
    .then((resp) => {
      state.recording.imageBusy = false;
      if (resp && resp.ok) {
        if (onSuccess) onSuccess();
      } else {
        state.recording.imageError =
          (resp && resp.error) || 'unknown error';
      }
      m.redraw();
    })
    .catch((err) => {
      state.recording.imageBusy = false;
      if (err && err.status === 503) {
        state.recording.imageError =
          'Image cache not configured — set library.imageRoot on the server.';
      } else {
        state.recording.imageError = errorMessage(err);
      }
      m.redraw();
    });
}

// runUpload pipes a picked File through the supplied upload endpoint,
// then (on success) re-fetches the recording detail so local_*_url
// fields pick up the newly-saved slot. Under the v2 layout the slot
// itself IS the chosen image — no follow-up "make this the selection"
// POST is needed.
//
// path — upload endpoint, server-relative including /api/v1.
// id   — the recording id, used to re-load detail after the upload.
function runUpload(path, id, file) {
  if (!file) return;
  if (state.recording.imageBusy) return;
  state.recording.imageBusy = true;
  state.recording.imageError = null;
  m.redraw();

  uploadFile(path, file)
    .then((resp) => {
      state.recording.imageBusy = false;
      if (!resp || resp.ok !== true) {
        state.recording.imageError = (resp && resp.error) || 'upload failed';
        m.redraw();
        return null;
      }
      // Re-fetch so local_*_url picks up the new slot.
      return loadRecording(id);
    })
    .catch((err) => {
      state.recording.imageBusy = false;
      if (err && err.status === 503) {
        state.recording.imageError =
          'Image cache not configured — set library.imageRoot on the server.';
      } else if (err && err.status === 413) {
        state.recording.imageError =
          'Upload too large — keep the file under 10 MB.';
      } else {
        state.recording.imageError = errorMessageFromUpload(err);
      }
      m.redraw();
    });
}

// runDangerAction handles a danger-zone button click: prompts for the
// typed enc-NNNN token, posts to the chosen endpoint, then re-fetches
// the recording so the buttons update. Inline failures land on
// state.recording.dangerError.
function runDangerAction(action, id) {
  state.recording.dangerError = '';
  const token = 'enc-' + id;
  const typed = window.prompt(
    'Type "' + token + '" to confirm. This action pushes upstream to Encora.',
    '',
  );
  if (typed == null) return; // User cancelled.
  if (String(typed).trim() !== token) {
    state.recording.dangerError =
      'Confirmation text did not match — nothing changed.';
    m.redraw();
    return;
  }

  state.recording.dangerBusy = true;
  m.redraw();
  api.post(action.path, {})
    .then((resp) => {
      state.recording.dangerBusy = false;
      if (resp && resp.ok) {
        // Re-fetch so the danger zone reflects the new collection/
        // wants membership. loadRecording triggers its own redraw.
        return loadRecording(id);
      }
      state.recording.dangerError =
        'Failed: ' + ((resp && resp.error) || 'unknown error');
      return null;
    })
    .catch((err) => {
      state.recording.dangerBusy = false;
      if (err && err.status === 503) {
        state.recording.dangerError =
          'Encora client not configured — set PROMPTBOOK_ENCORA_APIKEY ' +
          'on the server to enable upstream writes.';
        return;
      }
      if (err && err.status === 429) {
        state.recording.dangerError =
          'Rate-limited by Encora. Wait a minute and try again.';
        return;
      }
      state.recording.dangerError = 'Failed: ' + errorMessage(err);
    });
}

// ─── Render helpers ────────────────────────────────────────────────────

// WarnIcon mirrors the inline warning glyph the legacy NFT callout
// used. Inlined as a Mithril vnode so the component stays a single
// dependency-free file.
function WarnIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg', width: 20, height: 20,
    viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'aria-hidden': 'true', class: 'h-5 w-5 shrink-0',
  }, [
    m('path', { d: 'M12 3 2.5 20h19L12 3z' }),
    m('path', { d: 'M12 10v4' }),
    m('path', { d: 'M12 17.5v.1' }),
  ]);
}

// nftBadge renders the small "NFT" pill that sits next to the page
// title when the recording is gated. Returns null when the recording
// isn't NFT so the caller can drop it into a list directly.
function nftBadge(loaded) {
  const nft = (loaded.Recording && loaded.Recording.nft) || {};
  if (nft.nft_forever || nft.nft_date) {
    return m('span', { class: 'badge badge-warning badge-sm' }, 'NFT');
  }
  return null;
}

// renderNFTCallout returns the alert-warning callout for NFT
// recordings, or null when not applicable. The two copy variants are
// the exact strings the user corrected in the legacy file:
//   - permanent  → "Not For Trade — permanent." / "Do not share or
//                  trade this recording."
//   - date-bound → "Not For Trade — until YYYY-MM-DD." / "Do not
//                  share or trade this recording until the date passes."
// formatNFTDate produces the locale-independent "Month D, YYYY" form
// used in the legacy page; we keep that to avoid drifting from the
// previously approved copy.
function renderNFTCallout(loaded) {
  const nft = (loaded.Recording && loaded.Recording.nft) || {};
  if (nft.nft_forever) {
    return m('div', { role: 'alert', class: 'alert alert-warning' }, [
      WarnIcon(),
      m('div', [
        m('div', { class: 'font-semibold' }, 'Not For Trade — permanent.'),
        m('div', { class: 'text-sm opacity-80' },
          'Do not share or trade this recording.'),
      ]),
    ]);
  }
  if (nft.nft_date) {
    const when = new Date(nft.nft_date);
    if (!Number.isNaN(when.getTime()) && when.getTime() > Date.now()) {
      const stamp = formatNFTDate(nft.nft_date);
      return m('div', { role: 'alert', class: 'alert alert-warning' }, [
        WarnIcon(),
        m('div', [
          m('div', { class: 'font-semibold' },
            'Not For Trade — until ' + stamp + '.'),
          m('div', { class: 'text-sm opacity-80' },
            'Do not share or trade this recording until the date passes.'),
        ]),
      ]);
    }
  }
  return null;
}

// renderHeader is the top page section: status badge, optional NFT
// badge, show title, the Tour · date · master subtitle, and a small
// right-aligned action bar carrying the "Edit images" button that
// opens the picker modal.
function renderHeader(loaded) {
  const r = loaded.Recording;
  const status = statusForRecording(loaded);
  const meta = STATUS_META[status] || STATUS_META.orphan;
  const date = smartDate(
    r.date && r.date.full_date,
    r.date && r.date.month_known,
    r.date && r.date.day_known,
  );
  const subParts = [];
  if (r.tour) subParts.push(r.tour);
  if (date && date !== '—') subParts.push(date);
  if (r.master) subParts.push('master ' + r.master);
  const nft = nftBadge(loaded);
  return m('header', { class: 'space-y-2' }, [
    m('div', { class: 'flex items-start justify-between gap-3 flex-wrap' }, [
      m('div', { class: 'space-y-2 min-w-0' }, [
        m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
          m('span', { class: 'badge ' + meta.badge }, meta.label),
          nft,
          m('span', { class: 'text-sm font-mono opacity-60' },
            'enc-' + String(r.id)),
        ]),
        m('h1', { class: 'text-3xl font-semibold' }, r.show || '—'),
        subParts.length
          ? m('p', { class: 'text-sm opacity-70 font-mono' },
              subParts.join(' · '))
          : null,
      ]),
      m('div', { class: 'flex items-center gap-2 shrink-0' }, [
        renderEditImagesButton({
          onclick: () => {
            state.recording.pickerOpen = true;
            state.recording.pickerTab = 'poster';
          },
        }),
      ]),
    ]),
  ]);
}

// renderPosterCard shows the cached poster.jpg (the burned-in render
// output) or a neutral placeholder. Under v2 the chosen image is the
// only image — no index dance, no upstream fallback (the picker UI
// fetches options live in Phase 4).
function renderPosterCard(loaded) {
  const chosen = (loaded && loaded.local_poster_url) || '';
  const figure = chosen
    ? m('figure', m('img', {
        src: chosen,
        alt: (loaded.Recording.show || 'recording') + ' poster',
        class: 'w-full h-auto object-cover',
        loading: 'lazy',
      }))
    : m('figure', {
        class: 'aspect-[2/3] flex items-center justify-center ' +
               'bg-base-200 text-base-content/40 text-sm font-mono',
      }, 'no poster');
  return m('div', { class: 'card bg-base-100 shadow-sm overflow-hidden' },
    figure);
}

// metaRow renders one definition-list-style line in the metadata card.
function metaRow(label, value) {
  return m('div', { class: 'flex justify-between gap-4 py-1 text-sm' }, [
    m('span', { class: 'opacity-60' }, label),
    m('span', { class: 'font-mono text-right break-all' }, value || '—'),
  ]);
}

// renderMetadataCard summarises the recording's primary fields. Folder
// derives from the first version's path so mismatched files surface a
// recognisable parent directory.
function renderMetadataCard(loaded) {
  const r = loaded.Recording;
  const meta = r.metadata || {};
  const date = smartDate(
    r.date && r.date.full_date,
    r.date && r.date.month_known,
    r.date && r.date.day_known,
  );
  const formatStr = loaded.InCollection ? (loaded.Format || '—') : '—';
  const cataloged = loaded.InCollection && loaded.CollectedAt
    ? formatNFTDate(loaded.CollectedAt) || loaded.CollectedAt
    : '—';
  const folder = (loaded.Versions && loaded.Versions.length > 0)
    ? (dirname(loaded.Versions[0].FilePath) || '—')
    : '—';
  const encoraURL = 'https://encora.it/recordings/' +
    encodeURIComponent(String(r.id));
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('h2', { class: 'card-title text-base' }, 'Metadata'),
      m('div', { class: 'divide-y divide-base-200' }, [
        metaRow('Show', r.show),
        metaRow('Tour', r.tour),
        metaRow('Date', date),
        metaRow('Master', r.master),
        metaRow('Format', formatStr),
        metaRow('Gifting', meta.gifting_status),
        metaRow('Owners', meta.owners_count != null ? String(meta.owners_count) : '—'),
        metaRow('Wanters', meta.wanters_count != null ? String(meta.wanters_count) : '—'),
        metaRow('Cataloged', cataloged),
        metaRow('Folder', folder),
      ]),
      m('div', { class: 'card-actions justify-end' }, [
        m('a', {
          class: 'btn btn-sm btn-ghost',
          href: encoraURL,
          target: '_blank',
          rel: 'noopener noreferrer',
        }, 'Open on encora.it'),
      ]),
    ]));
}

// renderCastCard lists every performer with their role + per-recording
// status badge. Performer names link to /people/:id when the catalog
// has a canonical performer row; otherwise they render as plain text.
// Headshots aren't returned by the recordings endpoint today, so this
// page renders a neutral monogram-free row — the people detail page is
// where headshots live.
function renderCastCard(loaded) {
  const cast = loaded.Cast || [];
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('h2', { class: 'card-title text-base' }, 'Cast · ' + cast.length),
      cast.length === 0
        ? m('div', { class: 'opacity-60 text-sm' }, 'No cast recorded.')
        : m('ul', { class: 'divide-y divide-base-200' },
            cast.map((entry) => castRow(entry))),
    ]));
}

function castRow(entry) {
  const perf = entry.Performer || {};
  const char = entry.Character || {};
  const status = entry.Status;
  const name = perf.Name || '—';
  const role = char.Name || '—';
  const pid = perf.PerformerID;
  const nameNode = (pid && pid > 0)
    ? m('a', {
        class: 'link link-hover font-medium',
        href: '#',
        onclick: (ev) => {
          ev.preventDefault();
          m.route.set('/people/' + encodeURIComponent(String(pid)));
        },
      }, name)
    : m('span', { class: 'font-medium' }, name);
  return m('li', { class: 'py-2 flex items-start justify-between gap-3' }, [
    m('div', { class: 'min-w-0' }, [
      nameNode,
      m('div', { class: 'text-xs font-mono opacity-60 truncate' }, role),
    ]),
    status && status.Label
      ? m('span', { class: 'badge badge-ghost badge-sm shrink-0' }, status.Label)
      : null,
  ]);
}

// renderVersionsCard tabulates the local files behind the recording.
// Empty state matches the legacy copy distinction between "in
// collection but no files" and "no files registered".
function renderVersionsCard(loaded) {
  const versions = loaded.Versions || [];
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('h2', { class: 'card-title text-base' },
        'Local versions · ' + versions.length),
      versions.length === 0
        ? m('div', { class: 'opacity-60 text-sm' },
            loaded.InCollection
              ? 'No local files. The recording is in your collection but no version is registered.'
              : 'No local files registered for this recording.')
        : m('div', { class: 'overflow-x-auto' },
            m('table', { class: 'table table-sm' }, [
              m('thead', m('tr', [
                m('th', 'Path'),
                m('th', 'Format'),
                m('th', 'Codec'),
                m('th', 'Quality'),
                m('th', { class: 'text-right' }, 'Size'),
              ])),
              m('tbody', versions.map((v, idx) => versionRow(v, idx))),
            ])),
    ]));
}

function versionRow(v, idx) {
  const isPrimary = idx === 0;
  const name = basename(v.FilePath || '');
  const dir = dirname(v.FilePath || '');
  const format = v.FormatLabel || v.Container || '—';
  let codec = v.VideoCodec || '—';
  if (v.AudioCodec) codec += ' / ' + v.AudioCodec;
  const quality = v.Quality || '—';
  return m('tr', [
    m('td', { class: 'font-mono text-xs', title: v.FilePath || '' }, [
      isPrimary ? m('span', { class: 'text-warning mr-1' }, '★') : null,
      m('span', name || '—'),
      dir ? m('div', { class: 'opacity-60 text-[10px] truncate' }, dir) : null,
    ]),
    m('td', { class: 'font-mono text-xs' }, format),
    m('td', { class: 'font-mono text-xs' }, codec),
    m('td', { class: 'font-mono text-xs' }, quality),
    m('td', { class: 'font-mono text-xs text-right' },
      humanSize(v.FileSizeBytes)),
  ]);
}

// renderNFOCard surfaces the on-disk movie.nfo when the server read
// one. We deliberately skip the synthetic "PREVIEW" fallback the
// legacy page rendered — the new layout simply omits the card when
// there's no real file. This matches the user's preference for the
// SPA to only show first-class data.
function renderNFOCard(loaded) {
  const content = loaded.nfo_content || '';
  if (!content) return null;
  let note = 'from disk';
  const rel = relativeTime(loaded.nfo_modified_at);
  if (rel && rel !== '—') note += ' · modified ' + rel;
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('div', { class: 'flex items-center justify-between' }, [
        m('h2', { class: 'card-title text-base' }, 'NFO output'),
        m('span', { class: 'text-xs font-mono opacity-60' }, note),
      ]),
      m('pre', {
        class: 'bg-base-200 text-xs p-3 rounded overflow-x-auto whitespace-pre',
      }, content),
    ]));
}

// renderDangerZone renders the bottom card. State-driven: which button
// shows depends on the recording's current Encora membership. The
// destructive pair use btn-error styling; "Add to wants" is
// constructive and stays neutral.
function renderDangerZone(loaded) {
  const action = dangerActionFor(loaded);
  if (!action) return null;
  const id = loaded.Recording.id;
  const busy = state.recording.dangerBusy;
  const error = state.recording.dangerError;
  const btnClass = action.destructive
    ? 'btn btn-sm btn-error btn-outline'
    : 'btn btn-sm';
  return m('div', { class: 'space-y-3' }, [
    m('div', { class: 'divider my-2' }),
    m('section', { class: 'space-y-2' }, [
      m('h2', {
        class: 'text-xs uppercase tracking-wide opacity-60 font-semibold',
      }, 'Danger zone'),
      m('p', { class: 'text-sm opacity-80 max-w-2xl' }, action.description),
      m('div', { class: 'flex items-center gap-3' }, [
        m('button', {
          type: 'button',
          class: btnClass,
          disabled: busy,
          onclick: () => runDangerAction(action, id),
        }, busy ? 'Working…' : action.label),
        error
          ? m('span', { class: 'text-error text-sm' }, error)
          : null,
      ]),
    ]),
  ]);
}

// renderSlotPreview is the v2 picker tab body for a single slot
// (poster / fanart). Shows the current cached image (if any) plus an
// Upload button that posts to the matching slot endpoint. The browse-
// upstream-options UI lands in Phase 4; this preview is the minimal
// affordance the user needs to override the auto-fetched slot.
function renderSlotPreview({ url, alt, aspect, uploadLabel, uploadPath, id }) {
  const busy = state.recording.imageBusy;
  const figureClass = 'rounded overflow-hidden bg-base-200 ' +
    (aspect === 'backdrop' ? 'w-full aspect-video max-w-2xl' : 'w-48 aspect-[2/3]');
  return m('section', { class: 'space-y-3' }, [
    m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
      m('h3', { class: 'text-sm font-semibold' }, uploadLabel),
      renderUploadButton({
        label: 'Upload ' + (aspect === 'backdrop' ? 'fanart' : 'poster'),
        disabled: busy,
        onSelect: (file) => runUpload(uploadPath, id, file),
      }),
    ]),
    url
      ? m('figure', { class: figureClass }, m('img', {
          src: url,
          alt,
          class: 'w-full h-full object-cover',
          loading: 'lazy',
        }))
      : m('div', { class: 'opacity-60 text-sm' },
          'No image on disk yet — upload one, or wait for the next ' +
          'refresh-encora job to populate this slot. ' +
          'A live picker for upstream options lands in Phase 4.'),
  ]);
}

// renderPosterPicker is the poster tab. Single slot under v2 — no
// thumbnail strip, no green-ring index. Phase 4 will add live upstream
// option browsing; for now this is preview + upload.
function renderPosterPicker(loaded) {
  const id = loaded.Recording.id;
  return renderSlotPreview({
    url: loaded.local_poster_url || '',
    alt: (loaded.Recording.show || 'recording') + ' poster',
    aspect: 'poster',
    uploadLabel: 'Poster',
    uploadPath: '/api/v1/recordings/' + id + '/poster-upload',
    id,
  });
}

// renderBackdropPicker is the fanart tab. Same shape as the poster
// tab but the slot is fanart.jpg and the aspect is 16:9.
function renderBackdropPicker(loaded) {
  const id = loaded.Recording.id;
  return renderSlotPreview({
    url: loaded.local_fanart_url || '',
    alt: (loaded.Recording.show || 'recording') + ' fanart',
    aspect: 'backdrop',
    uploadLabel: 'Fanart',
    uploadPath: '/api/v1/recordings/' + id + '/fanart-upload',
    id,
  });
}

// renderOverlayEditor is the burned-in-text override subsection. The
// input is always populated — with the override when set, otherwise
// with the auto-derived fallback so the user can see what they're
// changing from. Save persists the typed value (even if it matches
// the fallback — explicit empty string allowed); Reset nulls the
// override and the renderer falls back to its computed string.
//
// Above the text editor sits a "Skip burn-in" toggle. When checked
// the renderer copies the raw selected backdrop verbatim to
// rendered.jpg (no compositing) and the text editor / Save / Reset
// affordances render disabled — they're irrelevant when no overlay
// is being baked. The two surfaces are independent persisted fields
// so flipping the toggle doesn't clobber the saved text override.
function renderOverlayEditor(loaded) {
  const id = loaded.Recording.id;
  const fallback = autoOverlayText(loaded);
  const isOverride = state.recording.overlayOverride != null;
  const draft = state.recording.overlayDraft != null
    ? state.recording.overlayDraft : '';
  const busy = state.recording.imageBusy;
  const disabled = !!state.recording.overlayDisabled;
  const editorDisabled = busy || disabled;
  return m('section', { class: 'space-y-2' }, [
    m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
      m('h3', { class: 'text-sm font-semibold' }, 'Overlay text'),
      disabled
        ? m('span', { class: 'badge badge-info badge-sm' }, 'burn-in off')
        : (isOverride
            ? m('span', { class: 'badge badge-warning badge-sm' }, 'override')
            : m('span', { class: 'badge badge-ghost badge-sm' }, 'auto')),
    ]),
    m('label', { class: 'label cursor-pointer justify-start gap-2 py-1' }, [
      m('input', {
        type: 'checkbox',
        class: 'toggle toggle-sm',
        checked: disabled,
        disabled: busy,
        onchange: (ev) => {
          const next = !!ev.target.checked;
          postPickerChoice(
            '/recordings/' + id + '/overlay-disabled',
            { disabled: next },
            () => { state.recording.overlayDisabled = next; },
          );
        },
      }),
      m('span', { class: 'label-text text-sm' },
        'Skip burn-in (use raw backdrop)'),
    ]),
    m('label', { class: 'input w-full' }, [
      m('input', {
        type: 'text',
        class: 'grow',
        value: draft,
        placeholder: fallback || 'show · tour · date',
        oninput: (ev) => {
          state.recording.overlayDraft = ev.target.value;
        },
        disabled: editorDisabled,
      }),
    ]),
    m('p', { class: 'text-xs opacity-60' },
      disabled
        ? 'Burn-in disabled · rendered.jpg is the raw backdrop, no label.'
        : 'Burned into the backdrop · Jellyfin/Plex see this label.'),
    m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
      m('button', {
        type: 'button',
        class: 'btn btn-sm btn-primary',
        disabled: editorDisabled,
        onclick: () => postPickerChoice(
          '/recordings/' + id + '/overlay',
          { text: draft, clear: false },
          () => { state.recording.overlayOverride = draft; },
        ),
      }, 'Save'),
      m('button', {
        type: 'button',
        class: 'btn btn-sm btn-ghost',
        disabled: editorDisabled || !isOverride,
        onclick: () => postPickerChoice(
          '/recordings/' + id + '/overlay',
          { clear: true },
          () => {
            state.recording.overlayOverride = null;
            state.recording.overlayDraft = autoOverlayText(loaded);
          },
        ),
      }, 'Reset to default'),
      isOverride || disabled
        ? null
        : m('span', { class: 'text-xs opacity-60' },
            'Default: ' + (fallback || '—')),
    ]),
  ]);
}

// renderImagePickerModal mounts the <dialog>-based modal that hosts
// the three picker subsections. The "Edit images" button in the page
// header toggles state.recording.pickerOpen; this component's
// onupdate hook syncs that flag to dialog.showModal()/close().
//
// After a successful POST inside the modal the page re-renders behind
// the dialog (state.recording.* mutates, m.redraw() fires) so the
// user sees the change immediately without us having to close the
// dialog or re-fetch.
function renderImagePickerModal(loaded) {
  const tabs = [
    { key: 'poster',   label: 'Poster',
      render: () => renderPosterPicker(loaded) },
    { key: 'backdrop', label: 'Backdrop',
      render: () => renderBackdropPicker(loaded) },
    { key: 'overlay',  label: 'Overlay text',
      render: () => renderOverlayEditor(loaded) },
  ];
  return m(ImagePickerModal, {
    open: !!state.recording.pickerOpen,
    onClose: () => { state.recording.pickerOpen = false; },
    title: 'Edit images',
    busy: !!state.recording.imageBusy,
    tabs,
    activeTab: state.recording.pickerTab || 'poster',
    onTabChange: (key) => { state.recording.pickerTab = key; },
  });
}

// renderBody composes the two-column main grid. Left column gathers
// the visual + reference cards (poster, metadata, NFT callout, NFO);
// right column carries the bigger informational surfaces (cast,
// versions). Falls back to a single column on small screens. The
// image-picker UI is no longer inline — it lives behind the
// "Edit images" button in the header and renders as a modal mounted
// at the page root from view().
function renderBody(loaded) {
  const callout = renderNFTCallout(loaded);
  const nfoCard = renderNFOCard(loaded);
  return m('div', { class: 'grid gap-6 lg:grid-cols-3' }, [
    m('div', { class: 'lg:col-span-1 space-y-6' }, [
      renderPosterCard(loaded),
      callout,
      renderMetadataCard(loaded),
    ]),
    m('div', { class: 'lg:col-span-2 space-y-6' }, [
      renderCastCard(loaded),
      renderVersionsCard(loaded),
      nfoCard,
    ]),
  ]);
}

// ─── Component ────────────────────────────────────────────────────────

const Recording = {
  oninit(vnode) {
    // Reset transient danger-zone + picker fields on every mount so a
    // stale error from the previous recording doesn't bleed through.
    state.recording.dangerBusy = false;
    state.recording.dangerError = '';
    state.recording.imageBusy = false;
    state.recording.imageError = null;
    state.recording.pickerOpen = false;
    state.recording.pickerTab = 'poster';
    const id = vnode.attrs && vnode.attrs.id;
    if (!id) {
      state.recording.loading = false;
      state.recording.error = new Error('Missing recording id.');
      return;
    }
    loadRecording(id);
  },

  // onupdate covers the case where the user navigates between
  // /recordings/:id urls without unmounting the component (Mithril
  // reuses the instance when the route resolver is the same). When
  // the id changes we re-fetch.
  onupdate(vnode) {
    const id = vnode.attrs && vnode.attrs.id;
    if (id && state.recording.id !== id) {
      state.recording.dangerBusy = false;
      state.recording.dangerError = '';
      state.recording.imageBusy = false;
      state.recording.imageError = null;
      state.recording.pickerOpen = false;
      state.recording.pickerTab = 'poster';
      loadRecording(id);
    }
  },

  view() {
    const rec = state.recording;
    if (rec.loading) {
      return m('div', { class: 'p-8 opacity-60' }, 'Loading recording…');
    }
    if (rec.error) {
      const err = rec.error;
      if (err && err.status === 404) {
        return m('div', { role: 'alert', class: 'alert alert-warning' }, [
          m('span', 'Recording not found.'),
        ]);
      }
      return m('div', { role: 'alert', class: 'alert alert-error' }, [
        m('span', 'Failed to load recording: ' +
          (err && err.message ? err.message : String(err))),
      ]);
    }
    const loaded = rec.loaded;
    if (!loaded || !loaded.Recording) {
      return m('div', { class: 'p-8 opacity-60' }, 'No recording data.');
    }
    return m('div', { class: 'space-y-6' }, [
      renderHeader(loaded),
      renderBody(loaded),
      renderDangerZone(loaded),
      renderImagePickerModal(loaded),
      renderImageErrorToast({
        error: state.recording.imageError,
        onDismiss: () => { state.recording.imageError = null; },
      }),
    ]);
  },
};

export default Recording;
