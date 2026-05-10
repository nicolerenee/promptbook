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
import graphql from '../graphql.js';
import state from '../state.js';
import { smartDate, humanSize, relativeTime, formatNFTDate } from '../utils/format.js';
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
// error. Mithril's m.request rejects with the parsed JSON body
// already deserialized (api.js stashes it on err.body as an object
// when the response was JSON, or as a string when it was plain text).
// Try the object path first, then the string-parse path, then fall
// back to err.message. Otherwise we'd render `[object Object]`
// straight to the user.
function errorMessage(err) {
  if (!err) return 'unknown error';
  const body = err.body;
  if (body && typeof body === 'object') {
    if (body.error) return String(body.error);
    if (body.message) return String(body.message);
  }
  if (typeof body === 'string') {
    try {
      const parsed = JSON.parse(body);
      if (parsed && parsed.error) return String(parsed.error);
      if (parsed && parsed.message) return String(parsed.message);
    } catch (_) { /* not JSON; fall through. */ }
    if (body.length < 240) return body;
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

// RECORDING_DETAIL_QUERY pulls the per-recording payload through the
// GraphQL surface: ent.Recording's denormalized fields plus the
// enrichment resolvers (status, localPosterURL, banner layout, NFO
// content + mtime, resolved cast, in-collection / in-wants /
// format strings driven through the reconciler). Image-picker /
// overlay-editor / banner-layout writes still go through the legacy
// REST POSTs — this only swaps the data fetch.
const RECORDING_DETAIL_QUERY = `
  query RecordingDetail($id: ID!) {
    recording(id: $id) {
      id
      tour
      master
      dateFull
      dateMonthKnown
      dateDayKnown
      rawJSON
      status
      inCollection
      inWants
      encoraFormat
      localFormatString
      collectedAt
      localPosterURL
      localFanartURL
      nfoContent
      nfoModifiedAt
      overlayDisabled
      overlayTextOverride
      bannerLayout {
        position
        imageRegion
      }
      resolvedCast {
        castEntryID
        performerID
        performerName
        performerSlug
        characterID
        characterName
        statusLabel
        statusAbbrev
        characterOrder
        localHeadshotURL
      }
      show {
        id
        name
      }
      versions(first: 100) {
        edges {
          node {
            id
            filePath
            container
            quality
            videoCodec
            audioCodec
            formatLabel
            fileSizeBytes
            addedAt
          }
        }
      }
    }
  }
`;

// stripIDPrefix turns "recording-1234" into "1234". URL routes
// (/recordings/:id) still carry int64s so we strip at the GraphQL
// boundary on the way back out to the renderer's existing keys.
function stripIDPrefix(id) {
  if (!id) return '';
  const idx = String(id).indexOf('-');
  return idx < 0 ? String(id) : String(id).substring(idx + 1);
}

// shapeRecordingDetail rebuilds the legacy storage.LoadedRecording +
// recordingDetailResponse merged shape from the GraphQL payload. The
// renderers below were written against PascalCase keys (the Go
// struct's zero-tag fallback); we reconstruct that shape so they keep
// reading correctly.
function shapeRecordingDetail(node) {
  if (!node) return null;
  const intID = Number(stripIDPrefix(node.id));
  const showID = node.show ? Number(stripIDPrefix(node.show.id)) : 0;
  const cast = (node.resolvedCast || []).map((ce) => ({
    Performer: {
      PerformerID: ce.performerID ? Number(stripIDPrefix(ce.performerID)) : 0,
      Name: ce.performerName || '',
      Slug: ce.performerSlug || '',
    },
    Character: {
      CharacterID: ce.characterID ? Number(stripIDPrefix(ce.characterID)) : 0,
      Name: ce.characterName || '',
      Order: ce.characterOrder || 0,
    },
    Status: ce.statusLabel
      ? { Label: ce.statusLabel, Abbreviation: ce.statusAbbrev || '' }
      : null,
    local_headshot_url: ce.localHeadshotURL || '',
  }));
  const versionEdges = (node.versions && node.versions.edges) || [];
  const versions = versionEdges
    .map((e) => e && e.node)
    .filter(Boolean)
    .map((v, i) => ({
      FilePath:      v.filePath || '',
      VideoCodec:    v.videoCodec || '',
      AudioCodec:    v.audioCodec || '',
      Quality:       v.quality || '',
      Container:     v.container || '',
      FormatLabel:   v.formatLabel || '',
      FileSizeBytes: v.fileSizeBytes || 0,
      Index:         i,
    }));
  // raw_json on the ent.Recording exposes the upstream JSON as a
  // string; the renderer expects the parsed shape on Recording.* so
  // we parse + flatten here. A parse failure leaves recording fields
  // empty rather than crashing the page.
  let parsed = null;
  if (node.rawJSON) {
    try { parsed = JSON.parse(node.rawJSON); } catch (_) { parsed = null; }
  }
  const rec = parsed || {};
  // Surface the canonical fields the renderer reads directly so a
  // malformed raw_json still produces a partial header instead of
  // nothing at all.
  rec.id = intID;
  rec.show = node.show ? node.show.name : (rec.show || '');
  rec.tour = node.tour || rec.tour || '';
  rec.master = node.master || rec.master || '';
  rec.date = rec.date || {
    full_date: node.dateFull || '',
    month_known: !!node.dateMonthKnown,
    day_known: !!node.dateDayKnown,
  };
  return {
    Recording:             rec,
    InCollection:          !!node.inCollection,
    InWants:               !!node.inWants,
    Format:                node.encoraFormat || '',
    LocalFormatString:     node.localFormatString || '',
    CollectedAt:           node.collectedAt || null,
    Versions:              versions,
    Cast:                  cast,
    showID,
    nfo_content:           node.nfoContent || '',
    nfo_modified_at:       node.nfoModifiedAt || null,
    local_poster_url:      node.localPosterURL || '',
    local_fanart_url:      node.localFanartURL || '',
    overlay_disabled:      !!node.overlayDisabled,
    overlay_text_override: node.overlayTextOverride == null ? null : node.overlayTextOverride,
    banner_layout: {
      position:     (node.bannerLayout && node.bannerLayout.position) || '',
      image_region: (node.bannerLayout && node.bannerLayout.imageRegion) || '',
    },
  };
}

// loadRecording fetches the recording detail payload via GraphQL and
// parks the shaped result on state.recording. Image-picker /
// overlay-editor / banner-layout writes still POST to the REST
// endpoints (not migrated in Phase 4b — those are mutations and
// remain on the existing surface).
function loadRecording(id) {
  state.recording.loading = true;
  state.recording.error = null;
  state.recording.id = id;
  state.recording.imageError = null;
  return graphql.query(RECORDING_DETAIL_QUERY, { id: 'recording-' + id })
    .then((data) => {
      const node = data && data.recording;
      if (!node) {
        const err = new Error('Recording not found.');
        err.status = 404;
        throw err;
      }
      return shapeRecordingDetail(node);
    })
    .then((shaped) => {
      state.recording.loaded = shaped;
      state.recording.loading = false;
      state.recording.overlayOverride =
        shaped.overlay_text_override != null
          ? shaped.overlay_text_override : null;
      state.recording.overlayDraft =
        state.recording.overlayOverride != null
          ? state.recording.overlayOverride
          : autoOverlayText(shaped);
      state.recording.overlayDisabled = !!shaped.overlay_disabled;
      const bl = shaped.banner_layout || {};
      state.recording.bannerPosition = bl.position || 'bottom';
      state.recording.bannerImageRegion = bl.image_region || 'middle';
    })
    .catch((err) => {
      state.recording.loaded = null;
      state.recording.error = err;
      state.recording.loading = false;
    });
}

// autoOverlayText derives the fallback overlay string the renderer
// burns in when no override is set. Mirrors imagerender.autoOverlayText
// server-side: line 1 is "Tour - Date", line 2 is "Venue, City". Show
// name is the title fallback when both tour and date are absent so the
// band never renders empty.
function autoOverlayText(loaded) {
  if (!loaded || !loaded.Recording) return '';
  const r = loaded.Recording;
  const tour = (r.tour || '').trim();
  const date = smartDate(
    r.date && r.date.full_date,
    r.date && r.date.month_known,
    r.date && r.date.day_known,
  );
  const dateStr = date && date !== '—' ? date : '';
  let title = joinSep(tour, dateStr, ' - ');
  if (!title) title = (r.show || '').trim();
  const meta = r.metadata || {};
  const venue = (meta.venue || '').trim();
  const city = (meta.city || '').trim();
  const subtitle = joinSep(venue, city, ', ');
  if (!title && !subtitle) return '';
  if (!subtitle) return title;
  if (!title) return subtitle;
  return title + '\n' + subtitle;
}

function joinSep(a, b, sep) {
  if (!a && !b) return '';
  if (!a) return b;
  if (!b) return a;
  return a + sep + b;
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

// stagePickerChoice records the URL the user clicked on for the given
// kind ('poster' | 'fanart'). The footer's Save button reads this back
// and runs commitPickerChoice to actually write the slot.
function stagePickerChoice(kind, url) {
  if (!state.recording.pickerStaged) state.recording.pickerStaged = {};
  state.recording.pickerStaged[kind] = url || null;
}

// commitPickerChoice POSTs the staged URL for kind into the matching
// from-url endpoint, bumps imageVersion on success, and re-loads the
// recording detail so local_*_url + the version cache-buster surface
// the new image without a manual page refresh. For the poster row
// the body also carries the picker's banner-layout selectors so the
// renderer's persisted style matches what the preview showed.
function commitPickerChoice(id, kind) {
  const staged = (state.recording.pickerStaged || {})[kind];
  if (!staged) return;
  const fromURLPath = '/recordings/' + id + '/' + kind + '-from-url';
  const body = { url: staged };
  if (kind === 'poster') {
    body.position = state.recording.bannerPosition || 'bottom';
    body.image_region = state.recording.bannerImageRegion || 'middle';
  }
  postPickerChoice(fromURLPath, body, () => {
    state.recording.imageVersion =
      (state.recording.imageVersion || 0) + 1;
    if (state.recording.pickerStaged) {
      state.recording.pickerStaged[kind] = null;
    }
    loadRecording(id);
  });
}

// withImageVersion appends ?v=<state.recording.imageVersion> so a
// just-saved image isn't masked by Safari's cached pre-save bytes at
// the same canonical /images/... path.
function withImageVersion(url) {
  if (!url) return url;
  const v = state.recording.imageVersion || 0;
  if (!v) return url;
  return url + (url.indexOf('?') >= 0 ? '&' : '?') + 'v=' + v;
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
      // Re-fetch so local_*_url picks up the new slot. Bump the
      // version counter so withImageVersion forces a fresh fetch
      // instead of serving the pre-upload bytes from cache.
      state.recording.imageVersion =
        (state.recording.imageVersion || 0) + 1;
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
            // Kick the upstream options fetch for the default tab so
            // the picker doesn't render a static skeleton until the
            // user clicks something.
            maybeLoadPickerOptions(loaded.Recording.id, 'poster');
          },
        }),
      ]),
    ]),
  ]);
}

// renderPosterCard shows the cached poster.jpg (the burned-in render
// output). Under v2 the chosen image is the only image — no index
// dance, no upstream fallback. local_poster_url is ALWAYS the
// canonical /images/... path; the server's /images/* route falls
// through to the SVG placeholder generator on cache miss, so the
// browser always gets a valid image. Empty url means image caching
// is disabled at the server level — show a "caching disabled" card.
function renderPosterCard(loaded) {
  const url = withImageVersion((loaded && loaded.local_poster_url) || '');
  const figure = url
    ? m('figure', m('img', {
        src: url,
        alt: (loaded.Recording.show || 'recording') + ' poster',
        class: 'w-full h-auto object-cover',
        loading: 'lazy',
      }))
    : m('figure', {
        class: 'aspect-[2/3] flex items-center justify-center ' +
               'bg-base-200 text-base-content/40 text-sm font-mono',
      }, 'image caching disabled');
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

// loadOptions fetches /api/v1/<entity>/<id>/<kind>-options and parks
// the result on state.recording.pickerOptions[kind]. Errors land on
// pickerOptionsError[kind] so the picker tab can render an inline
// alert. Loading flips pickerOptionsLoading[kind] for the duration.
function loadOptions(id, kind) {
  state.recording.pickerOptionsLoading = state.recording.pickerOptionsLoading || {};
  state.recording.pickerOptions = state.recording.pickerOptions || {};
  state.recording.pickerOptionsError = state.recording.pickerOptionsError || {};
  state.recording.pickerOptionsGen = state.recording.pickerOptionsGen || {};
  state.recording.pickerOptionsLoading[kind] = true;
  state.recording.pickerOptionsError[kind] = null;
  // Don't clear pickerOptions on refetch — the user prefers seeing the
  // stale strip while the new fetch flies than a flash to skeletons.
  m.redraw();

  return api.get('/recordings/' + encodeURIComponent(id) + '/' + kind + '-options')
    .then((body) => {
      state.recording.pickerOptions[kind] =
        (body && Array.isArray(body.options)) ? body.options : [];
      // Bump the generation so the picker forces fresh <img> requests
      // (cache-buster on the URL + Mithril key churn). Critical when a
      // previous fetch's images failed to load — without this the
      // browser keeps reusing the cached failure.
      state.recording.pickerOptionsGen[kind] =
        (state.recording.pickerOptionsGen[kind] || 0) + 1;
      state.recording.pickerOptionsLoading[kind] = false;
      m.redraw();
    })
    .catch((err) => {
      state.recording.pickerOptionsLoading[kind] = false;
      if (err && err.status === 503) {
        state.recording.pickerOptionsError[kind] =
          'Upstream client not configured on the server.';
      } else {
        state.recording.pickerOptionsError[kind] = errorMessage(err);
      }
      m.redraw();
    });
}

// runRefreshFromUpstream fires POST /recordings/:id/refresh-images
// (or /shows/:id/...). On success closes the modal + parks an info
// message on state so the toast renders. The user pulls the page
// down via m.route.set or wait for the runner to finish — this
// helper does NOT poll.
function runRefreshFromUpstream(id) {
  state.recording.imageBusy = true;
  m.redraw();
  api.post('/recordings/' + encodeURIComponent(id) + '/refresh-images',
    { force: true })
    .then(() => {
      state.recording.imageBusy = false;
      state.recording.pickerOpen = false;
      state.recording.imageInfo =
        'Refresh queued — page will update when the job completes.';
      // Auto-dismiss the info toast after 5s so the user isn't
      // stuck dismissing it manually for a routine confirmation.
      setTimeout(() => {
        if (state.recording.imageInfo) {
          state.recording.imageInfo = null;
          m.redraw();
        }
      }, 5000);
      m.redraw();
    })
    .catch((err) => {
      state.recording.imageBusy = false;
      if (err && err.status === 503) {
        state.recording.imageError =
          'Background jobs not configured on the server.';
      } else if (err && err.status === 409) {
        state.recording.imageError =
          'A refresh is already in flight — wait for it to finish.';
      } else {
        state.recording.imageError = errorMessage(err);
      }
      m.redraw();
    });
}

// renderPickerTab renders one of the live picker tabs. kind is
// 'poster' or 'fanart'; the helper resolves the right local URL,
// upstream-options endpoint, and from-url POST path off the kind.
//
// Click-to-stage: a thumbnail click sets state.recording.pickerStaged[kind]
// and the modal footer's "Save selection" button is what actually
// commits the choice. Uploads bypass staging because the file picker
// IS the explicit confirmation.
function renderPickerTab(loaded, kind) {
  const id = loaded.Recording.id;
  const localURL = kind === 'fanart'
    ? (loaded.local_fanart_url || '')
    : (loaded.local_poster_url || '');
  const aspect = kind === 'fanart' ? 'fanart' : 'poster';
  const uploadPath = '/api/v1/recordings/' + id + '/' + kind + '-upload';

  const optionsByKind = state.recording.pickerOptions || {};
  const loadingByKind = state.recording.pickerOptionsLoading || {};
  const errorByKind = state.recording.pickerOptionsError || {};
  const genByKind = state.recording.pickerOptionsGen || {};
  const stagedByKind = state.recording.pickerStaged || {};
  const stagedURL = stagedByKind[kind] || null;

  // Preview tile only on the poster row — fanart isn't burned-in,
  // so the staged thumbnail IS the preview already. The server
  // composites the overlay over the staged URL via
  // /poster-preview?url= and streams the result; the browser caches
  // it for the lifetime of the staged URL + selector values.
  let previewURL = null;
  const bannerPosition = state.recording.bannerPosition || 'bottom';
  const bannerRegion = state.recording.bannerImageRegion || 'middle';
  if (kind === 'poster' && stagedURL) {
    previewURL = '/api/v1/recordings/' + encodeURIComponent(id)
      + '/poster-preview?url=' + encodeURIComponent(stagedURL)
      + '&position=' + encodeURIComponent(bannerPosition)
      + '&region=' + encodeURIComponent(bannerRegion);
  }

  // Banner-layout selectors live on the poster tab only; fanart has
  // no overlay so position/region don't apply.
  const layoutSelectors = kind === 'poster'
    ? renderBannerLayoutSelectors(bannerPosition, bannerRegion)
    : null;

  return renderUpstreamPicker({
    currentURL: withImageVersion(localURL),
    currentLabel: kind === 'fanart' ? 'Current fanart' : 'Current poster',
    currentAlt: (loaded.Recording.show || 'recording') + ' ' + kind,
    aspect,
    options: optionsByKind[kind],
    loading: !!loadingByKind[kind],
    error: errorByKind[kind] || null,
    busy: !!state.recording.imageBusy,
    loadGen: genByKind[kind] || 0,
    staged: stagedURL,
    previewURL,
    layoutControls: layoutSelectors,
    onPick: (url) => stagePickerChoice(kind, url),
    onUpload: (file) => runUpload(uploadPath, id, file),
    onRefetch: () => loadOptions(id, kind),
    uploadLabel: kind === 'fanart' ? 'Upload fanart' : 'Upload poster',
  });
}

// renderBannerLayoutSelectors draws icon-button radio groups for the
// banner position + image-region choices, above the Current/Preview
// tile pair on the poster tab. Clicking an icon updates
// state.recording.banner* and triggers a redraw so the Preview URL
// re-builds with the new query params. Each group is a DaisyUI .join
// (segmented control) so the buttons read as one unit.
function renderBannerLayoutSelectors(position, region) {
  return m('div', { class: 'flex flex-wrap gap-6 mb-2' }, [
    m('div', { class: 'flex flex-col gap-1' }, [
      m('span', { class: 'text-xs opacity-60' }, 'Banner position'),
      iconRadioGroup({
        value: position,
        onChoose: (v) => { state.recording.bannerPosition = v; },
        options: [
          { value: 'top',    tip: 'Top',    icon: positionIcon('top') },
          { value: 'bottom', tip: 'Bottom', icon: positionIcon('bottom') },
        ],
      }),
    ]),
    m('div', { class: 'flex flex-col gap-1' }, [
      m('span', { class: 'text-xs opacity-60' },
        'Image area (which 80% to keep)'),
      iconRadioGroup({
        value: region,
        onChoose: (v) => { state.recording.bannerImageRegion = v; },
        // Order matches the user's mental model: cut-top, cut-both,
        // cut-bottom. The kept-region enum names invert (the icon
        // showing "top shaded out" means we KEEP the bottom).
        options: [
          { value: 'bottom', tip: 'Cut top 20%',
            icon: regionIcon('bottom') },
          { value: 'middle', tip: 'Cut top + bottom 10% each',
            icon: regionIcon('middle') },
          { value: 'top',    tip: 'Cut bottom 20%',
            icon: regionIcon('top') },
        ],
      }),
    ]),
  ]);
}

// iconRadioGroup renders a horizontal join of icon buttons; the
// option whose value matches `value` gets primary styling, the
// others fall back to ghost. Each button has a tooltip surfacing
// the human-readable label so the icon-only UI stays accessible.
function iconRadioGroup(attrs) {
  const { value, onChoose, options } = attrs;
  return m('div', { role: 'radiogroup', class: 'join' },
    options.map((opt) => {
      const active = opt.value === value;
      const cls = 'btn btn-sm join-item ' +
        (active ? 'btn-primary' : 'btn-ghost') + ' tooltip';
      return m('button', {
        type: 'button',
        class: cls,
        'data-tip': opt.tip,
        'aria-pressed': active ? 'true' : 'false',
        'aria-label': opt.tip,
        onclick: () => { if (!active) onChoose(opt.value); },
      }, opt.icon);
    }));
}

// positionIcon draws a small poster-shaped rectangle with a band
// stripe at the top or bottom — visually mirroring how the band
// will sit in the rendered poster.
function positionIcon(variant) {
  // Frame: 20×28 viewBox, ~3:4 aspect to read as a poster shape.
  const bandTop = variant === 'top' ? 1 : 22; // band y0
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg',
    width: 22, height: 28, viewBox: '0 0 20 28',
    fill: 'none', 'aria-hidden': 'true',
  }, [
    // Image rectangle outline.
    m('rect', {
      x: 1, y: 1, width: 18, height: 26, rx: 2,
      fill: 'currentColor', 'fill-opacity': 0.15,
      stroke: 'currentColor', 'stroke-width': 1.4,
    }),
    // Band stripe — thicker fill at the chosen edge.
    m('rect', {
      x: 1, y: bandTop, width: 18, height: 5,
      fill: 'currentColor', 'fill-opacity': 0.85,
    }),
  ]);
}

// regionIcon draws a small poster-shaped rectangle with the CUT
// portion shaded heavily and the kept portion light. variant names
// match the kept-region enum (top / middle / bottom), so the icon
// shows the COMPLEMENT — what gets cut by the band.
function regionIcon(variant) {
  // Cut bands are 5 px tall (≈20% of the 26-px tall image rect).
  // For "middle" we draw two 2.5-px bands at top + bottom.
  let cuts;
  if (variant === 'top') {
    // Keep top 80% → cut bottom 20%.
    cuts = [{ y: 22, h: 5 }];
  } else if (variant === 'bottom') {
    // Keep bottom 80% → cut top 20%.
    cuts = [{ y: 1, h: 5 }];
  } else {
    // Keep middle 80% → cut top + bottom 10% each.
    cuts = [{ y: 1, h: 2.6 }, { y: 24.4, h: 2.6 }];
  }
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg',
    width: 22, height: 28, viewBox: '0 0 20 28',
    fill: 'none', 'aria-hidden': 'true',
  }, [
    m('rect', {
      x: 1, y: 1, width: 18, height: 26, rx: 2,
      fill: 'currentColor', 'fill-opacity': 0.15,
      stroke: 'currentColor', 'stroke-width': 1.4,
    }),
    ...cuts.map((c) => m('rect', {
      x: 1, y: c.y, width: 18, height: c.h,
      fill: 'currentColor', 'fill-opacity': 0.85,
    })),
  ]);
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
        placeholder: fallback || 'Tour - Date\nVenue, City',
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
  const id = loaded.Recording.id;
  const tabs = [
    { key: 'poster',   label: 'Poster',
      render: () => renderPickerTab(loaded, 'poster') },
    { key: 'fanart',   label: 'Fanart',
      render: () => renderPickerTab(loaded, 'fanart') },
    { key: 'overlay',  label: 'Overlay text',
      render: () => renderOverlayEditor(loaded) },
  ];
  // Footer composition:
  //   - "Save selection" appears on the poster + fanart tabs and
  //     commits the staged URL (disabled when nothing is staged).
  //     Hidden on the overlay tab — that surface has its own inline
  //     Save / Reset buttons next to the textarea.
  //   - "Refresh from upstream" is always visible; it kicks the
  //     refresh-images job and is independent of the staged URL.
  const activeTab = state.recording.pickerTab || 'poster';
  const stagedByKind = state.recording.pickerStaged || {};
  const busy = !!state.recording.imageBusy;
  const footerActions = [];
  if (activeTab === 'poster' || activeTab === 'fanart') {
    footerActions.push({
      label: 'Save selection',
      primary: true,
      disabled: !stagedByKind[activeTab] || busy,
      onClick: () => commitPickerChoice(id, activeTab),
    });
  }
  footerActions.push({
    label: 'Refresh from upstream',
    primary: false,
    disabled: busy,
    onClick: () => runRefreshFromUpstream(id),
  });
  return m(ImagePickerModal, {
    open: !!state.recording.pickerOpen,
    onClose: () => {
      state.recording.pickerOpen = false;
      // Drop staged selections on close so reopening the modal doesn't
      // resurrect a half-applied choice from a previous session.
      state.recording.pickerStaged = {};
    },
    title: 'Edit images',
    busy,
    tabs,
    activeTab,
    onTabChange: (key) => {
      state.recording.pickerTab = key;
      maybeLoadPickerOptions(id, key);
    },
    footerActions,
  });
}

// maybeLoadPickerOptions kicks the options fetch on first switch to
// a picker tab. Subsequent switches re-render against the cached
// options; the user can hit "Re-fetch" in the picker body to force a
// fresh fetch.
function maybeLoadPickerOptions(id, tab) {
  if (tab !== 'poster' && tab !== 'fanart') return;
  const cache = state.recording.pickerOptions || {};
  if (Array.isArray(cache[tab])) return; // already loaded.
  loadOptions(id, tab);
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
    state.recording.imageInfo = null;
    state.recording.pickerOpen = false;
    state.recording.pickerTab = 'poster';
    state.recording.pickerOptions = {};
    state.recording.pickerOptionsLoading = {};
    state.recording.pickerOptionsError = {};
    state.recording.pickerStaged = {};
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
      state.recording.imageInfo = null;
      state.recording.pickerOpen = false;
      state.recording.pickerTab = 'poster';
      state.recording.pickerOptions = {};
      state.recording.pickerOptionsLoading = {};
      state.recording.pickerOptionsError = {};
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
      renderImageInfoToast({
        message: state.recording.imageInfo,
        onDismiss: () => { state.recording.imageInfo = null; },
      }),
    ]);
  },
};

export default Recording;
