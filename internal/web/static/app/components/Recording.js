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
import {
  smartDate,
  smartDateWithVariant,
  humanSize,
  relativeTime,
  formatNFTDate,
} from '../utils/format.js';
import {
  uploadFile,
  errorMessageFromUpload,
} from '../utils/uploadPicker.js';
import ImagePickerModal, {
  renderImageErrorToast,
  renderImageInfoToast,
  renderUpstreamPicker,
} from './ImagePickerModal.js';
import RecordingRenameModal, {
  loadRenamePreview,
} from './RecordingRenameModal.js';

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
      localReleaseFormat
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
            partIndex
            addedAt
          }
        }
      }
      mediaInfo {
        container
        videoCodec
        width
        height
        videoBitDepth
        videoFps
        durationSeconds
        scanType
        audioStreams {
          codec
          channelLayout
          bitrate
          language
        }
        subtitleStreams {
          codec
          language
        }
      }
      extras {
        path
        name
        sizeBytes
        isDir
        kind
        label
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
      // PartIndex is 0 for single-file versions and >=1 for the
      // n-th part of a multipart recording. The Versions table
      // collapses rows that share a recording_id and carry
      // PartIndex >= 1 into a single primary row.
      PartIndex:     v.partIndex || 0,
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
    // LocalReleaseFormat is the canonical "what files do you have"
    // display string built locally from each version's MediaInfo
    // blob. Replaces the legacy free-text encora release_format
    // field as the metadata-card "Release format" line. See the
    // releaseformat package on the server for the format spec.
    LocalReleaseFormat:    node.localReleaseFormat || '',
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
    // mediaInfo is the Sonarr-style ffprobe blob. null when the
    // recording has no version row yet (orphan, missing) or the blob
    // wasn't captured (legacy import). The card renders only when
    // present.
    media_info:            node.mediaInfo || null,
    // extras lists the non-main files in the version's source folder
    // — folder-as-unit drops only. Loose-file imports surface [].
    extras:                Array.isArray(node.extras) ? node.extras : [],
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

// REGENERATE_NFO_MUTATION rewrites movie.nfo from scratch using the
// current data + image + media-info state. Thin wrapper around the
// nforefresh.Service.RewriteForRecording fan-out — fire-and-forget
// from the SPA's perspective; we surface the outcome via the inline
// toast pair on state.recording.
const REGENERATE_NFO_MUTATION = `
  mutation RegenerateRecordingNFO($id: ID!) {
    regenerateRecordingNFO(recordingID: $id) {
      ok
      error
    }
  }
`;

// runRegenerateNFO fires the regenerateRecordingNFO mutation and parks
// the outcome on state.recording. Success → a confirmation toast that
// auto-dismisses; failure → an inline error toast the user must
// dismiss manually. Disables the button via state.recording.
// regeneratingNFO for the duration so a double-click can't fire two
// concurrent mutations.
function runRegenerateNFO(id) {
  if (state.recording.regeneratingNFO) return;
  state.recording.regeneratingNFO = true;
  state.recording.regenerateNFOMessage = null;
  state.recording.regenerateNFOError = null;
  m.redraw();

  graphql.query(REGENERATE_NFO_MUTATION, { id: 'recording-' + id })
    .then((data) => {
      const resp = (data && data.regenerateRecordingNFO) || {};
      state.recording.regeneratingNFO = false;
      if (resp.ok) {
        state.recording.regenerateNFOMessage = 'NFO regenerated.';
        // Bump the image version so the NFO card re-fetches its
        // mtime + content via the recording detail re-load below.
        loadRecording(id);
        // Auto-dismiss the success toast after 4s.
        setTimeout(() => {
          if (state.recording.regenerateNFOMessage) {
            state.recording.regenerateNFOMessage = null;
            m.redraw();
          }
        }, 4000);
      } else {
        state.recording.regenerateNFOError =
          resp.error || 'Regenerate NFO failed.';
      }
      m.redraw();
    })
    .catch((err) => {
      state.recording.regeneratingNFO = false;
      state.recording.regenerateNFOError =
        (err && err.message) || 'Regenerate NFO failed.';
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

// ─── Toolbar icons (Heroicons Outline 24/24, inlined) ─────────────────
//
// Each helper returns a small SVG vnode at size-4 so it sits cleanly
// next to a btn-sm label. Inline rather than imported — the SPA bundle
// is hand-rolled and we only need a handful of glyphs. stroke uses
// currentColor so DaisyUI button variants colour them automatically.

function svgIcon(paths) {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg',
    fill: 'none',
    viewBox: '0 0 24 24',
    'stroke-width': 1.5,
    stroke: 'currentColor',
    'aria-hidden': 'true',
    class: 'size-4',
  }, paths);
}

function refreshIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99',
    }),
  ]);
}

function pencilSquareIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M16.862 4.487 18.549 2.799a2.121 2.121 0 1 1 3 3L19.862 7.487m-3-3L6.34 15.01a4.5 4.5 0 0 0-1.13 1.897l-1.06 3.708 3.71-1.06a4.5 4.5 0 0 0 1.896-1.13L19.862 7.487m-3-3 3 3M9 19.5h6.75',
    }),
  ]);
}

function documentArrowPathIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M19.5 14.25v-2.625a3.375 3.375 0 0 0-3.375-3.375h-1.5A1.125 1.125 0 0 1 13.5 7.125v-1.5a3.375 3.375 0 0 0-3.375-3.375H8.25m2.25 0H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 0 0-9-9Z',
    }),
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M9 14.25 7.5 15.75 9 17.25m6-3 1.5 1.5-1.5 1.5',
    }),
  ]);
}

function photoIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'm2.25 15.75 5.159-5.159a2.25 2.25 0 0 1 3.182 0l5.159 5.159m-1.5-1.5 1.409-1.409a2.25 2.25 0 0 1 3.182 0l2.909 2.909m-18 3.75h16.5a1.5 1.5 0 0 0 1.5-1.5V6a1.5 1.5 0 0 0-1.5-1.5H3.75A1.5 1.5 0 0 0 2.25 6v12a1.5 1.5 0 0 0 1.5 1.5Zm10.5-11.25h.008v.008h-.008V8.25Zm.375 0a.375.375 0 1 1-.75 0 .375.375 0 0 1 .75 0Z',
    }),
  ]);
}

function clockIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M12 6v6h4.5m4.5 0a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
    }),
  ]);
}

function trashIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'm14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0',
    }),
  ]);
}

function externalLinkIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25',
    }),
  ]);
}

// bookmarkIcon is the "save for later" glyph for the Add-to-wants
// button. Visually pairs with the destructive trash icon — when the
// recording flips between collection / wants / orphan the toolbar
// swaps which of the two it shows.
function bookmarkIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M17.593 3.322c1.1.128 1.907 1.077 1.907 2.185V21L12 17.25 4.5 21V5.507c0-1.108.806-2.057 1.907-2.185a48.507 48.507 0 0 1 11.186 0Z',
    }),
  ]);
}

// chevronRightIcon is the inline expand/collapse glyph used by the
// per-version Media Info disclosure rows in the Files section. The
// rotation is handled by a CSS transform tied to expanded state on
// the row, so the same SVG handles both states.
function chevronRightIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'm8.25 4.5 7.5 7.5-7.5 7.5',
    }),
  ]);
}

// folderIcon labels Extras section directory rows. Plain outline so
// it reads as a tree-control affordance rather than a stylistic flair.
function folderIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M2.25 12.75V12A2.25 2.25 0 0 1 4.5 9.75h15A2.25 2.25 0 0 1 21.75 12v.75m-8.69-6.44-2.12-2.12a1.5 1.5 0 0 0-1.061-.44H4.5A2.25 2.25 0 0 0 2.25 6v12a2.25 2.25 0 0 0 2.25 2.25h15A2.25 2.25 0 0 0 21.75 18V9a2.25 2.25 0 0 0-2.25-2.25h-5.379a1.5 1.5 0 0 1-1.06-.44Z',
    }),
  ]);
}

// fileIcon labels Extras section regular-file rows.
function fileIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M19.5 14.25v-2.625a3.375 3.375 0 0 0-3.375-3.375h-1.5A1.125 1.125 0 0 1 13.5 7.125v-1.5a3.375 3.375 0 0 0-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 0 0-9-9Z',
    }),
  ]);
}

// infoIcon is the per-version Media Info expander affordance in the
// Versions table. Stays small (size-4 via svgIcon) so it sits inline
// with the row data without dominating it.
function infoIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'm11.25 11.25.041-.02a.75.75 0 0 1 1.063.852l-.708 2.836a.75.75 0 0 0 1.063.853l.041-.021M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9-3.75h.008v.008H12V8.25Z',
    }),
  ]);
}

// stripHTML strips tags from a description blob. Encora's
// metadata.show_description ships with WYSIWYG-flavoured <p>/<br>/&quot;
// markup; we only need plain text for the hero's plot block. Mirrors
// nfo/writer.go's stripHTML on the server side; kept local to avoid a
// shared utility for one consumer.
function stripHTML(s) {
  if (!s) return '';
  // Replace block-level closes with newlines so paragraph breaks
  // survive the tag strip; collapse the rest of the markup.
  let out = String(s)
    .replace(/<\/(p|div|br|li)\s*>/gi, '\n')
    .replace(/<br\s*\/?\s*>/gi, '\n')
    .replace(/<[^>]+>/g, '');
  // Decode the handful of HTML entities Encora actually emits.
  out = out
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, '&')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&nbsp;/g, ' ');
  return out.replace(/\n{3,}/g, '\n\n').trim();
}

// runRefreshFull fires the aggregate refresh-recording-full scheduled
// job via RunNow. The job re-pulls the Encora detail document,
// re-probes every on-disk version, rewrites the movie.nfo, and kicks
// the per-recording image refresh — all in one click. After the POST
// resolves we wait briefly (matching the queue page's scan trigger
// cadence) and refetch the recording detail so any DB changes from
// the upstream re-pull surface immediately. The full pass with
// Encora + probes + NFO + image fan-out can take longer than the
// refetch delay; the user can refetch manually if needed.
function runRefreshFull(id) {
  if (state.recording.imageBusy) return;
  state.recording.imageBusy = true;
  state.recording.imageError = null;
  m.redraw();
  api.post('/jobs/scheduled/refresh-recording-full/run', {
    args: { recording_id: id },
  })
    .then(() => {
      state.recording.imageInfo =
        'Refresh queued — page will update when the job completes.';
      setTimeout(() => {
        if (state.recording.imageInfo) {
          state.recording.imageInfo = null;
          m.redraw();
        }
      }, 5000);
      // Refetch after a short delay so the typical metadata + nfo
      // steps have landed by the time the page re-renders.
      return new Promise((resolve) => setTimeout(resolve, 1500))
        .then(() => loadRecording(id));
    })
    .catch((err) => {
      if (err && err.status === 503) {
        state.recording.imageError =
          'Background jobs not configured on the server.';
      } else if (err && err.status === 409) {
        state.recording.imageError =
          'A refresh is already in flight — wait for it to finish.';
      } else {
        state.recording.imageError = errorMessage(err);
      }
    })
    .then(() => {
      state.recording.imageBusy = false;
      m.redraw();
    });
}

// ellipsisIcon is the three-dots glyph used by the More-actions
// dropdown trigger in the header. Heroicons Outline.
function ellipsisIcon() {
  return svgIcon([
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M6.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0ZM12.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0ZM18.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0Z',
    }),
  ]);
}

// chevronDownIcon is the small caret used by the inline Links
// dropdown in the header subtitle row. Smaller (size-3) so it sits
// flush with the surrounding badge-sm pill.
function chevronDownIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg',
    fill: 'none',
    viewBox: '0 0 24 24',
    'stroke-width': 1.5,
    stroke: 'currentColor',
    'aria-hidden': 'true',
    class: 'size-3',
  }, [
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'm19.5 8.25-7.5 7.5-7.5-7.5',
    }),
  ]);
}

// linkIcon is the small chain-link glyph used by the inline Links
// dropdown trigger.
function linkIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg',
    fill: 'none',
    viewBox: '0 0 24 24',
    'stroke-width': 1.5,
    stroke: 'currentColor',
    'aria-hidden': 'true',
    class: 'size-3.5',
  }, [
    m('path', {
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      d: 'M13.19 8.688a4.5 4.5 0 0 1 1.242 7.244l-4.5 4.5a4.5 4.5 0 0 1-6.364-6.364l1.757-1.757m13.35-.622 1.757-1.757a4.5 4.5 0 0 0-6.364-6.364l-4.5 4.5a4.5 4.5 0 0 0 1.242 7.244',
    }),
  ]);
}

// renderLinksDropdown is the inline "Links" affordance in the header
// subtitle row. Surfaces every external pointer the recording carries
// behind a single DaisyUI dropdown so the subtitle row stays compact
// even when more sources land later (Stagemedia, IMDB, …). Each menu
// item opens in a new tab. Today the only entry is Encora.
function renderLinksDropdown(id) {
  const links = [
    {
      label: 'Encora',
      href: 'https://encora.it/recordings/' + encodeURIComponent(String(id)),
    },
  ];
  return m('div', { class: 'dropdown dropdown-end' }, [
    m('div', {
      tabindex: 0,
      role: 'button',
      class: 'badge badge-ghost gap-1 cursor-pointer',
      'aria-label': 'Links',
    }, [linkIcon(), m('span', 'Links'), chevronDownIcon()]),
    m('ul', {
      tabindex: 0,
      class: 'dropdown-content menu menu-sm z-10 mt-1 w-44 rounded-box ' +
             'bg-base-100 shadow border border-base-200 p-2',
    }, links.map((l) => m('li',
      m('a', {
        href: l.href,
        target: '_blank',
        rel: 'noopener noreferrer',
      }, [externalLinkIcon(), m('span', l.label)])))),
  ]);
}

// renderActionsCluster is the icon-button trio that anchors the
// top-right of the header info column. Mirrors the show page's
// "Edit images" affordance — Refresh + Edit are bare icon buttons,
// the rest (rename / NFO / history / danger) collapse into a
// three-dots More-menu so the action surface stops dominating the
// header.
function renderActionsCluster(loaded) {
  const id = loaded.Recording.id;
  const action = dangerActionFor(loaded);
  const refreshBusy = !!state.recording.imageBusy;
  const regenBusy = !!state.recording.regeneratingNFO;
  const dangerBusy = !!state.recording.dangerBusy;

  const refreshBtn = m('button', {
    type: 'button',
    class: 'btn btn-sm btn-ghost btn-square tooltip tooltip-bottom',
    'aria-label': 'Refresh',
    'data-tip': 'Refresh',
    disabled: refreshBusy,
    onclick: () => runRefreshFull(id),
  }, refreshBusy
    ? m('span', { class: 'loading loading-spinner loading-xs' })
    : refreshIcon());

  const editBtn = m('button', {
    type: 'button',
    class: 'btn btn-sm btn-ghost btn-square tooltip tooltip-bottom',
    'aria-label': 'Edit images',
    'data-tip': 'Edit images',
    onclick: () => {
      state.recording.pickerOpen = true;
      state.recording.pickerTab = 'poster';
      maybeLoadPickerOptions(id, 'poster');
    },
  }, photoIcon());

  // Build the More-menu items. Rename + NFO + History are always
  // present; the danger-zone entry is state-driven through
  // dangerActionFor — destructive actions render with error styling,
  // the constructive "Add to wants" branch with primary styling, and
  // a null action drops the row entirely.
  const menuItems = [
    m('li', m('a', {
      onclick: (ev) => {
        ev.preventDefault();
        state.recording.renameOpen = true;
        state.recording.renamePreview = null;
        state.recording.renameResult = null;
        state.recording.renameApplyError = null;
        loadRenamePreview(id);
      },
    }, [pencilSquareIcon(), m('span', 'Preview rename')])),
    m('li', m('a', {
      onclick: (ev) => {
        ev.preventDefault();
        // Apply rename routes through the same modal as Preview —
        // the modal is where the user confirms + runs the apply, so
        // a separate "direct apply" entry would skip the
        // confirmation users expect. Same flow as Preview rename.
        state.recording.renameOpen = true;
        state.recording.renamePreview = null;
        state.recording.renameResult = null;
        state.recording.renameApplyError = null;
        loadRenamePreview(id);
      },
    }, [pencilSquareIcon(), m('span', 'Apply rename')])),
    m('li', { class: regenBusy ? 'disabled' : '' }, m('a', {
      onclick: (ev) => {
        ev.preventDefault();
        if (regenBusy) return;
        runRegenerateNFO(id);
      },
    }, [
      regenBusy
        ? m('span', { class: 'loading loading-spinner loading-xs' })
        : documentArrowPathIcon(),
      m('span', regenBusy ? 'Regenerating…' : 'Regenerate NFO'),
    ])),
    m('li', m('a', {
      onclick: (ev) => {
        ev.preventDefault();
        m.route.set('/history', { recording_id: String(id) });
      },
    }, [clockIcon(), m('span', 'History')])),
  ];
  if (action) {
    const isDestructive = !!action.destructive;
    menuItems.push(m('li',
      { class: 'border-t border-base-200 mt-1 pt-1' + (dangerBusy ? ' disabled' : '') },
      m('a', {
        class: isDestructive ? 'text-error' : 'text-primary',
        onclick: (ev) => {
          ev.preventDefault();
          if (dangerBusy) return;
          runDangerAction(action, id);
        },
      }, [
        dangerBusy
          ? m('span', { class: 'loading loading-spinner loading-xs' })
          : (isDestructive ? trashIcon() : bookmarkIcon()),
        m('span', dangerBusy ? 'Working…' : action.label),
      ])));
  }

  const moreBtn = m('div', { class: 'dropdown dropdown-end' }, [
    m('div', {
      tabindex: 0,
      role: 'button',
      class: 'btn btn-sm btn-ghost btn-square',
      'aria-label': 'More actions',
    }, ellipsisIcon()),
    m('ul', {
      tabindex: 0,
      class: 'dropdown-content menu menu-sm z-10 mt-1 w-56 rounded-box ' +
             'bg-base-100 shadow border border-base-200 p-2',
    }, menuItems),
  ]);

  return m('div', { class: 'flex items-center gap-1 shrink-0' },
    [refreshBtn, editBtn, moreBtn]);
}

// renderHeader is the per-recording header card: poster on the left,
// metadata column on the right, action cluster anchored top-right.
// Mirrors the show detail page's renderHeader so the two surfaces
// read as variants of the same component — clean white-card-on-base
// feel, no fanart-as-background hero treatment, no overlay.
//
// Layout (top-down inside the info column):
//   - Eyebrow:   "RECORDING · enc-N" (small, uppercase, muted)
//   - Title:     <h1> with the Show name (linked to /shows/:id)
//   - Subtitle:  "{Tour} · {DateWithVariant}" + master/Pro-Shot badge
//                + Links dropdown
//   - Media:     "{LocalReleaseFormat} · {runtime}" mono line
//   - Plot:      stripped show_description (whitespace-pre-line)
//   - Badges:    Status / Gifting / Trading / Owners / Wanters
function renderHeader(loaded) {
  const r = loaded.Recording;
  const id = r.id;
  const posterURL = withImageVersion(loaded.local_poster_url ||
    '/images/recordings/' + id + '/poster.jpg');

  // Title — just the Show name. Clickable link to /shows/:id when we
  // have a show id; plain span otherwise.
  const showName = r.show || '—';
  const showID = loaded.showID;
  const titleNode = (showID && showID > 0)
    ? m('a', {
        class: 'link link-hover',
        href: '#',
        onclick: (ev) => {
          ev.preventDefault();
          m.route.set('/shows/' + encodeURIComponent(String(showID)));
        },
      }, showName)
    : m('span', showName);

  // Subtitle text — "Tour · DateWithVariant". Either piece may be
  // empty; joinSep collapses to whichever side has content. The
  // variant comes from raw_json.date.date_variant — see
  // smartDateWithVariant().
  const date = smartDateWithVariant(
    r.date && r.date.full_date,
    r.date && r.date.month_known,
    r.date && r.date.day_known,
    r.date && r.date.date_variant,
  );
  const dateStr = date && date !== '—' ? date : '';
  const subtitleText = joinSep(r.tour || '', dateStr, ' · ');

  // Master / Pro-Shot badge. Pro-shot recordings get a single warning
  // badge regardless of master string (the master is irrelevant for
  // broadcast); other recordings show the bare master string in a
  // ghost badge. Both branches return null when there's nothing to
  // render so the row collapses cleanly.
  const meta = r.metadata || {};
  const recordingType = meta.recording_type || '';
  let masterBadge = null;
  if (recordingType === 'pro-shot') {
    masterBadge = m('span', { class: 'badge badge-warning badge-sm' }, 'Pro-Shot');
  } else if (r.master) {
    masterBadge = m('span', { class: 'badge badge-ghost badge-sm' }, r.master);
  }

  // Media info line — "{LocalReleaseFormat} · {runtime}" in a muted
  // mono font. Either segment may be empty; the whole line is hidden
  // when both are missing. The format part keeps the mono face so
  // codecs/resolution line up; the runtime stays plain.
  const mi = loaded.media_info;
  const runtime = (mi && mi.durationSeconds > 0)
    ? formatMediaRunTime(mi.durationSeconds) : '';
  const releaseFormat = loaded.LocalReleaseFormat || '';
  let mediaInfoLine = null;
  if (releaseFormat || runtime) {
    const parts = [];
    if (releaseFormat) {
      parts.push(m('span', { class: 'font-mono' }, releaseFormat));
    }
    if (releaseFormat && runtime) {
      parts.push(m('span', { class: 'opacity-60' }, ' · '));
    }
    if (runtime) parts.push(m('span', runtime));
    mediaInfoLine = m('div',
      { class: 'text-sm opacity-70' }, parts);
  }

  // Badge row (bottom of info column) — Status / Gifting / Trading /
  // Owners / Wanters. The enc-{id} chip moved to the eyebrow; the NFT
  // pill (when present) sits at the end so the destructive cue
  // anchors the row.
  const status = statusForRecording(loaded);
  const statusMeta = STATUS_META[status] || STATUS_META.orphan;
  const owners = (meta.owners_count != null) ? Number(meta.owners_count) : 0;
  const wanters = (meta.wanters_count != null) ? Number(meta.wanters_count) : 0;
  // The standalone NFT pill is gone — tradingBadge already covers the
  // NFT state (red until the date, green after, green when no NFT).
  // A duplicate "NFT" chip sat next to "No Trading until ..." and
  // duplicated the same signal.
  const badgeRow = [
    m('span', { class: 'badge ' + statusMeta.badge }, statusMeta.label),
    giftingBadge(meta.gifting_status || ''),
    tradingBadge(loaded),
    owners > 0
      ? m('span', { class: 'badge badge-ghost badge-sm' },
          String(owners) + ' owners')
      : null,
    wanters > 0
      ? m('span', { class: 'badge badge-ghost badge-sm' },
          String(wanters) + ' wants')
      : null,
  ];

  // Plot — show the recording's trading/general notes (the per-
  // recording free-text the trader wrote about THIS specific capture
  // — washout, intermission notes, cast tidbits, etc.). The show
  // description is the same for every recording of the show, so it's
  // not useful here — but the NFO writer joins both for jellyfin's
  // <plot>. Empty notes leaves the plot collapsed.
  const plot = stripHTML(r.notes || '');

  // Subtitle row — Tour · Date text, followed by the master/Pro-Shot
  // chip and an inline Links dropdown. flex-wrap so narrow viewports
  // can stack the trailing chips beneath the text rather than
  // overflowing.
  const subtitleRow = m('div', {
    class: 'flex flex-wrap items-center gap-2 text-sm opacity-70',
  }, [
    subtitleText
      ? m('span', { class: 'font-mono' }, subtitleText)
      : null,
    masterBadge,
    renderLinksDropdown(id),
  ]);

  return m('div', {
    class: 'card lg:card-side bg-base-100 shadow-sm overflow-hidden',
  }, [
    posterURL
      ? m('figure', { class: 'lg:w-64 shrink-0' }, m('img', {
          src: posterURL,
          alt: showName + ' poster',
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
        m('div', { class: 'min-w-0 space-y-1' }, [
          m('div', {
            class: 'text-xs uppercase tracking-wider opacity-60',
          }, 'Recording · enc-' + String(id)),
          m('h1', { class: 'text-3xl font-semibold leading-tight' },
            titleNode),
          subtitleRow,
          mediaInfoLine,
        ]),
        renderActionsCluster(loaded),
      ]),
      plot
        ? m('p', {
            class: 'text-base sm:text-lg max-w-3xl whitespace-pre-line opacity-80',
          }, plot)
        : null,
      m('div', { class: 'flex flex-wrap gap-2 mt-2' }, badgeRow),
      state.recording.dangerError
        ? m('div', { role: 'alert', class: 'alert alert-error mt-2' },
            m('span', { class: 'text-sm' }, state.recording.dangerError))
        : null,
    ]),
  ]);
}

// giftingBadge maps the encora gifting_status string to a coloured
// badge. Returns null for empty / unknown so the badge row collapses.
// "Always Gift (no trading)" loses the parenthetical (the trading
// half is rendered as a separate "No Trading" badge in tradingBadge).
function giftingBadge(status) {
  if (!status) return null;
  switch (status) {
  case 'Never Gift':
    return m('span', { class: 'badge badge-error' }, 'Never Gift');
  case 'Always Gift (no trading)':
    return m('span', { class: 'badge badge-success' }, 'Always Gift');
  case 'Gift On Request':
    return m('span', { class: 'badge badge-success' }, 'Gift on Request');
  case 'Gift at your Discretion':
    return m('span', { class: 'badge badge-success' }, 'Gift at Discretion');
  default:
    return null;
  }
}

// tradingBadge combines gifting_status + nft to render the trading
// pill. Hard "No Trading" when the gifting status forbids it or NFT
// is set forever. NFT-with-date keeps the "No Trading until {date}"
// text either way but flips colour: warning yellow while the date is
// in the future, success green once it's past — the constraint is
// over but the audit-trail label still tells the user when it
// expired. "Trading OK" when no constraints apply. Always returns a
// badge so the row carries a clear trading-state signal.
function tradingBadge(loaded) {
  const r = loaded.Recording || {};
  const meta = r.metadata || {};
  const nft = r.nft || {};
  if ((meta.gifting_status || '') === 'Always Gift (no trading)') {
    return m('span', { class: 'badge badge-error' }, 'No Trading');
  }
  if (nft.nft_forever) {
    return m('span', { class: 'badge badge-error' }, 'No Trading');
  }
  if (nft.nft_date) {
    // Truncate to YYYY-MM-DD — server stores the field as ISO date /
    // RFC3339 timestamp depending on origin; either way the date
    // portion is the leading 10 chars.
    const stamp = String(nft.nft_date).substring(0, 10);
    // Date-only ISO parses as UTC midnight; comparing to Date.now()
    // gives the correct "is the constraint still active" answer
    // regardless of the user's timezone.
    const expiry = Date.parse(stamp);
    const past = !isNaN(expiry) && expiry <= Date.now();
    const colour = past ? 'badge-success' : 'badge-warning';
    return m('span', { class: 'badge ' + colour },
      'No Trading until ' + stamp);
  }
  return m('span', { class: 'badge badge-success' }, 'Trading OK');
}

// renderCastCard lists every performer as a portrait card in a
// horizontally-scrolling DaisyUI tideflyer — Radarr-style. Cards hold
// a 2:3 portrait headshot (matching the placeholder generator's
// aspect ratio so missing-headshot performers fill the frame the same
// as real headshots), name, role string (already prefixed with u/s,
// s/w, etc. by the cast resolver), and route to /people/:id on click.
// Native scroll-button is the affordance — no chevron buttons, since
// the page already carries plenty of UI complexity.
function renderCastCard(loaded) {
  const cast = loaded.Cast || [];
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body' }, [
      m('h2', { class: 'card-title text-base' }, 'Cast · ' + cast.length),
      cast.length === 0
        ? m('div', { class: 'opacity-60 text-sm' }, 'No cast recorded.')
        : m('div', {
            class: 'tideflyer tideflyer-center w-full p-2 space-x-3 rounded-box bg-base-200/40',
          }, cast.map((entry, idx) => castTideflyerItem(entry, idx))),
    ]));
}

// castTideflyerItem renders one performer card. Width is fixed at
// w-32 (128px) so a typical desktop viewport fits ~7-8 cards before
// the strip needs to scroll. The portrait frame is aspect-[2/3] so
// the placeholder SVG and real headshot both crop the same way via
// object-cover.
function castTideflyerItem(entry, idx) {
  const perf = entry.Performer || {};
  const char = entry.Character || {};
  const status = entry.Status;
  const name = perf.Name || '—';
  const role = char.Name || '';
  const pid = perf.PerformerID;
  const headshot = entry.local_headshot_url || '';
  const clickable = !!(pid && pid > 0);
  const onclick = clickable
    ? (ev) => {
        ev.preventDefault();
        m.route.set('/people/' + encodeURIComponent(String(pid)));
      }
    : null;
  // The role line carries the status prefix (u/s, s/w, etc.) when
  // the resolver attached one — but the resolver also emits a
  // separate Status.Label for badge use elsewhere. Render the role
  // verbatim; if it's empty fall back to the status label so a
  // performer with no character mapping still gets a meaningful
  // second line.
  const roleLine = role || (status && status.Label) || '';
  return m('div', {
    key: 'cast-' + idx,
    class: 'tideflyer-item',
  }, m('a', {
    class: 'block w-32 group ' + (clickable ? 'cursor-pointer' : 'cursor-default'),
    href: clickable ? '/people/' + encodeURIComponent(String(pid)) : '#',
    onclick: onclick,
    'aria-label': clickable ? 'Open ' + name : name,
  }, [
    m('div', {
      class: 'aspect-[2/3] w-full overflow-hidden rounded-md bg-base-300',
    }, headshot
      ? m('img', {
          src: headshot,
          alt: name,
          loading: 'lazy',
          class: 'w-full h-full object-cover ' +
            (clickable ? 'transition-opacity group-hover:opacity-90' : ''),
        })
      : null),
    m('div', { class: 'mt-2 font-semibold text-sm break-words leading-tight' }, name),
    roleLine
      ? m('div', { class: 'text-xs opacity-70 break-words leading-tight' }, roleLine)
      : null,
  ]));
}

// groupVersionsForTable buckets the per-version edges into rendering
// groups for the Versions table. Versions with PartIndex == 0 each
// produce a singleton group (today's one-row-per-version behaviour);
// versions with PartIndex >= 1 collapse into a single multipart group
// regardless of count — the recording detail page is already scoped to
// one recording, so all "I'm part N of something" rows belong to the
// same multipart version. Parts inside a group are sorted by
// PartIndex so "Parts 1, 2" reads correctly even when the wire ordering
// is something else.
function groupVersionsForTable(versions) {
  const groups = [];
  const partVersions = [];
  versions.forEach((v, idx) => {
    if ((v.PartIndex || 0) >= 1) {
      partVersions.push({ v: v, idx: idx });
    } else {
      groups.push({ kind: 'single', items: [{ v: v, idx: idx }] });
    }
  });
  if (partVersions.length > 0) {
    partVersions.sort((a, b) => (a.v.PartIndex || 0) - (b.v.PartIndex || 0));
    groups.push({ kind: 'multipart', items: partVersions });
  }
  return groups;
}

// renderVersionsTable tabulates the recording's local files. Each
// group is one visual primary row: singletons render today's
// one-row-per-version layout, multipart groups (PartIndex >= 1)
// collapse into a single primary row showing summed size + a parts
// badge, with a "Show parts" disclosure underneath. Per-row Media
// Info expansion lives on state.recording.expandedVersions keyed by
// the group's lead index — the disclosure stays stable across redraws
// because Mithril keys by the lead index.
function renderVersionsTable(loaded) {
  const versions = loaded.Versions || [];
  const mi = loaded && loaded.media_info;
  if (versions.length === 0) {
    return m('div', { class: 'opacity-60 text-sm' },
      loaded.InCollection
        ? 'No local files. The recording is in your collection but no version is registered.'
        : 'No local files registered for this recording.');
  }
  const expandedMap = state.recording.expandedVersions || {};
  const groups = groupVersionsForTable(versions);
  const rows = [];
  groups.forEach((g) => {
    const leadIdx = g.items[0].idx;
    const expanded = !!expandedMap[leadIdx];
    if (g.kind === 'multipart') {
      rows.push(multipartVersionRow(g.items, leadIdx, mi, expanded));
    } else {
      rows.push(versionRow(g.items[0].v, leadIdx, mi, expanded));
    }
    if (expanded) {
      rows.push(m('tr', { key: 'mi-' + leadIdx }, [
        m('td', {
          colspan: 6,
          class: 'bg-base-200/40',
        }, m('div', { class: 'px-2' }, renderMediaInfoBody(mi))),
      ]));
    }
  });
  return m('div', { class: 'overflow-x-auto' },
    m('table', { class: 'table table-sm' }, [
      m('thead', m('tr', [
        m('th', 'Path'),
        m('th', 'Format'),
        m('th', 'Codec'),
        m('th', 'Quality'),
        m('th', { class: 'text-right' }, 'Size'),
        // Sixth column houses the per-row info-icon disclosure
        // affordance. Header stays empty so the row labels read
        // cleanly; aria-label on the button covers screen readers.
        m('th', { class: 'w-8', 'aria-label': 'Actions' }),
      ])),
      m('tbody', rows),
    ]));
}

// versionDisplayCells builds the shared Format / Codec / Quality
// cells used by both versionRow and multipartVersionRow. Pulled out
// so the multipart primary row reads exactly like the singleton row
// for the metadata columns — parts share format/codec/quality, so
// rendering off part 1 (the lead) is sufficient.
function versionDisplayCells(v, mi) {
  const format = v.Container || (mi && mi.container) || '—';
  let videoCodec = v.VideoCodec || (mi && formatVideoCodec(mi.videoCodec)) || '';
  let audioCodec = v.AudioCodec || '';
  if (!audioCodec && mi && mi.audioStreams && mi.audioStreams[0]) {
    audioCodec = String(mi.audioStreams[0].codec || '').toUpperCase();
  }
  let codec = videoCodec || '—';
  if (audioCodec) codec += ' / ' + audioCodec;
  let quality = v.Quality;
  if (!quality && mi) quality = qualityFromHeight(mi.height);
  if (!quality) quality = '—';
  return { format: format, codec: codec, quality: quality };
}

// versionInfoButton renders the per-row Media Info disclosure icon
// + handler. Shared between singleton and multipart primary rows so
// the affordance is identical regardless of grouping.
function versionInfoButton(idx, expanded) {
  return m('button', {
    type: 'button',
    class: 'btn btn-ghost btn-xs',
    'aria-label': expanded ? 'Hide media info' : 'Show media info',
    'aria-expanded': expanded ? 'true' : 'false',
    title: expanded ? 'Hide media info' : 'Show media info',
    onclick: () => {
      if (!state.recording.expandedVersions) {
        state.recording.expandedVersions = {};
      }
      state.recording.expandedVersions[idx] = !expanded;
    },
  }, infoIcon());
}

// versionRow falls back to the recording-level MediaInfo when the
// per-version typed columns (Container/VideoCodec/Quality/AudioCodec)
// are empty — pre-mediainfo imports never populated those columns,
// so without the fallback the card showed three em-dashes for files
// whose real codec/quality is plain visible in the Media Info detail
// behind the disclosure icon.
function versionRow(v, idx, mi, expanded) {
  const isPrimary = idx === 0;
  const name = basename(v.FilePath || '');
  const dir = dirname(v.FilePath || '');
  // Format = container (just "MP4"/"MKV"). Drops the legacy
  // FormatLabel string because it bundled size into the same field
  // and produced "MP4 - 8.57 GB" — the Size column already shows it.
  const cells = versionDisplayCells(v, mi);
  return m('tr', { key: 'v-' + idx }, [
    m('td', { class: 'font-mono text-xs', title: v.FilePath || '' }, [
      isPrimary ? m('span', { class: 'text-warning mr-1' }, '★') : null,
      m('span', name || '—'),
      dir ? m('div', { class: 'opacity-60 text-[10px] truncate' }, dir) : null,
    ]),
    m('td', { class: 'font-mono text-xs' }, cells.format),
    m('td', { class: 'font-mono text-xs' }, cells.codec),
    m('td', { class: 'font-mono text-xs' }, cells.quality),
    m('td', { class: 'font-mono text-xs text-right' },
      humanSize(v.FileSizeBytes)),
    m('td', { class: 'text-right w-8' },
      versionInfoButton(idx, expanded)),
  ]);
}

// multipartVersionRow collapses N part rows (PartIndex >= 1) into a
// single primary row showing the parent folder, a "N parts" badge
// listing the part numbers, and the summed file size. A "Show parts"
// <details> sub-row reveals each part's filename + size in
// part-index order. The shared format/codec/quality columns read off
// part 1's typed values (parts share characteristics) and the per-row
// Media Info expander shows the lead part's MediaInfo blob —
// duplicating it per part would clutter the table without adding info.
function multipartVersionRow(items, idx, mi, expanded) {
  const lead = items[0].v;
  const dir = dirname(lead.FilePath || '');
  const parts = items.map((it) => it.v.PartIndex || 0);
  const partsLabel = parts.length + ' parts';
  const partsList = parts.join(', ');
  const totalSize = items.reduce(
    (acc, it) => acc + (it.v.FileSizeBytes || 0), 0);
  const cells = versionDisplayCells(lead, mi);
  return m('tr', { key: 'vmp-' + idx }, [
    m('td', { class: 'font-mono text-xs' }, [
      m('div', { class: 'flex items-center gap-2 flex-wrap' }, [
        m('span', { class: 'text-warning' }, '★'),
        m('span', dir || '—'),
        m('span', {
          class: 'badge badge-info badge-sm',
          title: 'Parts ' + partsList,
        }, partsLabel),
      ]),
      m('details', { class: 'mt-1' }, [
        m('summary', {
          class: 'cursor-pointer text-[10px] opacity-60 select-none',
        }, 'Show parts'),
        m('ul', { class: 'mt-1 space-y-0.5 pl-3' },
          items.map((it) => m('li', {
            class: 'flex items-center justify-between gap-3',
          }, [
            m('span', { class: 'font-mono text-[10px] truncate',
              title: it.v.FilePath || '' }, [
              m('span', { class: 'opacity-60 mr-1' },
                'Part ' + (it.v.PartIndex || 0) + ':'),
              m('span', basename(it.v.FilePath || '') || '—'),
            ]),
            m('span', {
              class: 'opacity-60 text-[10px] font-mono shrink-0',
            }, humanSize(it.v.FileSizeBytes || 0)),
          ]))),
      ]),
    ]),
    m('td', { class: 'font-mono text-xs' }, cells.format),
    m('td', { class: 'font-mono text-xs' }, cells.codec),
    m('td', { class: 'font-mono text-xs' }, cells.quality),
    m('td', { class: 'font-mono text-xs text-right' },
      humanSize(totalSize)),
    m('td', { class: 'text-right w-8' },
      versionInfoButton(idx, expanded)),
  ]);
}

// renderExtrasSection lists every non-main file in the recording's
// source folder, grouping the output as a one-level tree: top-level
// files render as plain rows; top-level directories render as
// collapsible <details> nodes whose summary carries a child-count
// badge. Phase 2 caps the visual depth at one level — a directory's
// children render flat under it rather than recursively building
// nested <details> for grand-children. Hidden entirely when
// loaded.extras is empty (loose-file imports, missing source folder).
function renderExtrasSection(loaded) {
  const extras = loaded.extras || [];
  if (extras.length === 0) return null;
  // Bucket the flat list into top-level files + a directory-keyed map
  // of children. The wire shape is already sorted by path, so a
  // single pass is enough — directories appear before their children
  // and children appear in path-sorted order under their parent.
  const topFiles = [];
  const dirChildren = new Map();
  const dirNames = [];
  for (const e of extras) {
    if (!e || typeof e.path !== 'string') continue;
    const slashIdx = e.path.indexOf('/');
    if (slashIdx < 0) {
      if (e.isDir) {
        dirNames.push(e.path);
        if (!dirChildren.has(e.path)) dirChildren.set(e.path, []);
      } else {
        topFiles.push(e);
      }
      continue;
    }
    const root = e.path.substring(0, slashIdx);
    if (!dirChildren.has(root)) {
      dirChildren.set(root, []);
      dirNames.push(root);
    }
    dirChildren.get(root).push(e);
  }
  return m('section', { class: 'space-y-2' }, [
    m('h3', { class: 'text-sm font-semibold opacity-80' },
      'Extras · ' + extras.length),
    m('div', { class: 'divide-y divide-base-200 rounded border border-base-200' }, [
      ...topFiles.map((e) => extraFileRow(e, e.name || basename(e.path))),
      ...dirNames.map((name) => extraDirSection(name, dirChildren.get(name) || [])),
    ]),
  ]);
}

// extrasKindBadge is the per-row hint chip showing what KIND of
// extra a file is — Featurette / Scene / BTS / Interview / Trailer /
// Deleted for Jellyfin-typed extras (badge-info), Audio / Photo /
// Other for the non-Jellyfin kinds promptbook tracks anyway
// (badge-ghost). Returns null for legacy rows where kind is empty so
// the row reads plain. badge-sm keeps the row height stable across
// kinds.
function extrasKindBadge(kind) {
  if (!kind) return null;
  const map = {
    featurette:      { label: 'Featurette', cls: 'badge-info' },
    scene:           { label: 'Scene',      cls: 'badge-info' },
    behindthescenes: { label: 'BTS',        cls: 'badge-info' },
    interview:       { label: 'Interview',  cls: 'badge-info' },
    trailer:         { label: 'Trailer',    cls: 'badge-info' },
    deletedscenes:   { label: 'Deleted',    cls: 'badge-info' },
    other:           { label: 'Other',      cls: 'badge-ghost' },
    audio:           { label: 'Audio',      cls: 'badge-ghost' },
    photo:           { label: 'Photo',      cls: 'badge-ghost' },
  };
  const meta = map[kind];
  if (!meta) return null;
  return m('span', {
    class: 'badge ' + meta.cls + ' badge-sm shrink-0',
    title: kind,
  }, meta.label);
}

// extraFileRow is one terminal row in the Extras section — a single
// file with its basename + size. The icon is purely decorative so the
// row reads cleanly even without it. A small kind badge sits to the
// right of the filename when the row carries a typed kind (post
// recording_extras-table imports); legacy untyped rows render plain.
function extraFileRow(entry, displayName) {
  const badge = extrasKindBadge(entry && entry.kind);
  return m('div', {
    class: 'flex items-center justify-between gap-3 px-3 py-1.5 text-sm',
  }, [
    m('div', { class: 'flex items-center gap-2 min-w-0' }, [
      m('span', { class: 'opacity-60 shrink-0' }, fileIcon()),
      m('span', { class: 'font-mono truncate', title: entry.path },
        displayName),
      badge,
    ]),
    m('span', { class: 'opacity-60 text-xs font-mono shrink-0' },
      humanSize(entry.sizeBytes || 0)),
  ]);
}

// extraDirSection wraps a directory + its (already-flat) children in a
// native <details>/<summary> pair. Mithril leaves the open state on
// the DOM element so a redraw doesn't collapse a directory the user
// expanded.
function extraDirSection(name, children) {
  // Children rows are stripped of the leading "{name}/" prefix so the
  // displayed label is just the file's basename — the summary line
  // already carries the directory context.
  const prefix = name + '/';
  return m('details', { class: 'group' }, [
    m('summary', {
      class: 'flex items-center justify-between gap-3 px-3 py-1.5 ' +
             'text-sm cursor-pointer hover:bg-base-200/40 list-none',
    }, [
      m('div', { class: 'flex items-center gap-2 min-w-0' }, [
        m('span', { class: 'opacity-60 shrink-0' }, folderIcon()),
        m('span', { class: 'font-mono truncate' }, name),
      ]),
      m('span', { class: 'badge badge-ghost badge-sm shrink-0' },
        '+' + children.length),
    ]),
    m('div', { class: 'pl-6 border-l border-base-200 ml-4 my-1' },
      children.map((c) => {
        const display = c.path.startsWith(prefix)
          ? c.path.substring(prefix.length)
          : c.name || c.path;
        return extraFileRow(c, display);
      })),
  ]);
}

// renderDetailsSection surfaces the per-recording catalog metadata
// the hero doesn't already carry — Cataloged timestamp + Folder path.
// Gifting / Owners / Wanters moved to the hero badge row; this
// section is intentionally short. Empty values fall through to "—".
function renderDetailsSection(loaded) {
  const versions = loaded.Versions || [];
  // Folder = the canonical destination folder for the recording's
  // first version. The SPA only carries dirname(FilePath) on
  // versions, so the displayed folder is always destination-side.
  const folder = versions.length > 0 ? dirname(versions[0].FilePath || '') : '';
  const cataloged = loaded.CollectedAt || '';
  const rows = [
    ['Cataloged', cataloged],
  ];
  return m('section', { class: 'space-y-2' }, [
    m('h2', { class: 'text-base font-semibold opacity-80' }, 'Details'),
    m('dl', {
      class: 'grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-1 text-sm',
    },
      rows.flatMap(([label, value]) => [
        m('dt', { class: 'opacity-60' }, label + ':'),
        m('dd', { class: 'font-mono break-all' }, value || '—'),
      ])),
    folder
      ? m('div', { class: 'text-sm flex flex-wrap gap-2 items-baseline pt-1' }, [
          m('span', { class: 'opacity-60' }, 'Folder:'),
          m('span', { class: 'font-mono break-all' }, folder),
        ])
      : null,
  ]);
}

// renderFilesSection collapses phase 1's three trailing cards (Local
// versions, Media Info, NFO output) into one Files surface containing
// the Versions table (with per-row Media Info expansion), the optional
// Extras tree, and the NFO disclosure row at the bottom.
function renderFilesSection(loaded) {
  const versions = loaded.Versions || [];
  const extras = loaded.extras || [];
  return m('div', { class: 'card bg-base-100 shadow-sm' },
    m('div', { class: 'card-body space-y-4' }, [
      m('h2', { class: 'card-title text-base' },
        'Files · ' + versions.length),
      renderVersionsTable(loaded),
      extras.length > 0 ? renderExtrasSection(loaded) : null,
      m('div', { class: 'border-t border-base-200 pt-2' },
        renderNFORow(loaded)),
    ]));
}

// formatMediaRunTime turns a duration in seconds into Sonarr's
// "H:MM:SS" / "MM:SS" form. Hours are dropped when zero so a 44-min
// recording reads "44:04" rather than "0:44:04".
function formatMediaRunTime(seconds) {
  const total = Math.floor(Math.max(0, Number(seconds) || 0));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n) => String(n).padStart(2, '0');
  if (h > 0) return h + ':' + pad(m) + ':' + pad(s);
  return pad(m) + ':' + pad(s);
}

// formatBitrate turns a bps integer into "317 kbps" — Sonarr's render.
// Uses kbps with no decimals; bitrates in our domain are 96–640 kbps so
// the precision drop is invisible.
function formatBitrate(bps) {
  const n = Number(bps) || 0;
  if (n <= 0) return '—';
  return Math.round(n / 1000) + ' kbps';
}

// qualityFromHeight is the same height→label map probe.MediaInfo's
// Quality() helper applies server-side. Used as a fallback in the
// versions table when the per-version typed column is empty (legacy
// imports before mediainfo plumbing landed).
function qualityFromHeight(h) {
  const n = Number(h) || 0;
  if (n >= 2160) return '2160p';
  if (n >= 1440) return '1440p';
  if (n >= 1080) return '1080p';
  if (n >= 720)  return '720p';
  if (n >= 480)  return '480p';
  if (n >= 360)  return '360p';
  if (n >= 240)  return '240p';
  return '';
}

// formatVideoCodec maps ffprobe codec names onto the labels Sonarr
// surfaces — h264 → x264, hevc → x265 — so users see the names they
// recognize from release groups. Unknown codecs pass through verbatim.
function formatVideoCodec(codec) {
  if (!codec) return '';
  switch (codec.toLowerCase()) {
    case 'h264':
      return 'x264';
    case 'hevc':
    case 'h265':
      return 'x265';
    default:
      return codec;
  }
}

// formatScanType maps ffprobe field_order onto the human label. The
// progressive case is overwhelmingly common; tt/bb/tb/bt indicate
// interlaced streams and surface as a single "Interlaced" bucket
// because the user doesn't care which field comes first.
function formatScanType(scan) {
  if (!scan) return '—';
  const lower = String(scan).toLowerCase();
  if (lower === 'progressive') return 'Progressive';
  if (lower === 'tt' || lower === 'bb' || lower === 'tb' || lower === 'bt') {
    return 'Interlaced';
  }
  // Defensive: anything else (rare) renders capitalized for legibility.
  return lower.charAt(0).toUpperCase() + lower.slice(1);
}

// joinUnique returns a "/"-separated list of the input strings,
// preserving order but dropping duplicates and empty values. Mirrors
// Sonarr's "eng/eng" rendering for two English audio streams.
function joinUnique(values, sep) {
  const seen = new Set();
  const out = [];
  for (const v of values || []) {
    if (!v) continue;
    if (seen.has(v)) continue;
    // Sonarr renders duplicates verbatim ("eng/eng") so the user can
    // see the per-stream count. Keep duplicates by NOT adding to seen
    // when the caller passes the raw list. Callers that want dedup
    // pass through Set-deduplicated input.
    seen.add(v);
    out.push(v);
  }
  return out.join(sep || '/');
}

// audioLanguagesDisplay returns the Sonarr-style "eng/eng" form: one
// token per audio stream, tokens NOT deduplicated, empty strings
// dropped. Matches the reference card the user pasted.
function audioLanguagesDisplay(streams) {
  const tokens = (streams || [])
    .map((s) => s && s.language ? s.language : '')
    .filter(Boolean);
  if (tokens.length === 0) return '—';
  return tokens.join('/');
}

// audioChannelsDisplay joins each stream's channelLayout — falling
// through to "{n}-channel" semantics is already handled server-side.
// Multiple streams join with " / " so 5.1 + stereo reads "5.1 / stereo".
function audioChannelsDisplay(streams) {
  const labels = (streams || [])
    .map((s) => s && s.channelLayout ? s.channelLayout : '')
    .filter(Boolean);
  if (labels.length === 0) return '—';
  return labels.join(' / ');
}

// subtitleLanguagesDisplay returns the unique "/"-joined subtitle
// languages.
function subtitleLanguagesDisplay(streams) {
  return joinUnique(
    (streams || []).map((s) => s && s.language ? s.language : ''),
    '/',
  );
}

// mediaInfoRows builds the Sonarr-style label/value pairs the recording
// detail page renders inside the per-version Media Info disclosure
// sub-row. Phase 2 lifted these out of a standalone card and into an
// inline expansion under each Versions table row, so the renderer is
// label-agnostic — the caller wraps these rows in whatever layout the
// surrounding context wants. Returns [] when mediaInfo is null
// (legacy imports, no version row, or unparseable blob); the caller
// hides the disclosure affordance in that case.
function mediaInfoRows(mi) {
  if (!mi) return [];
  const audio = mi.audioStreams || [];
  const subs = mi.subtitleStreams || [];
  const firstAudio = audio[0] || null;
  const rows = [];
  if (firstAudio && firstAudio.bitrate > 0) {
    rows.push(['Audio Bitrate', formatBitrate(firstAudio.bitrate)]);
  }
  rows.push(['Audio Channels', audioChannelsDisplay(audio)]);
  if (firstAudio && firstAudio.codec) {
    rows.push(['Audio Codec', String(firstAudio.codec).toUpperCase()]);
  }
  rows.push(['Audio Languages', audioLanguagesDisplay(audio)]);
  rows.push(['Audio Stream Count', String(audio.length)]);
  rows.push(['Video Bit Depth', String(mi.videoBitDepth || 0)]);
  rows.push(['Video Codec', formatVideoCodec(mi.videoCodec) || '—']);
  if (mi.videoFps && mi.videoFps > 0) {
    rows.push(['Video Fps', mi.videoFps.toFixed(3)]);
  } else {
    rows.push(['Video Fps', '—']);
  }
  if (mi.width > 0 && mi.height > 0) {
    rows.push(['Resolution', mi.width + 'x' + mi.height]);
  } else {
    rows.push(['Resolution', '—']);
  }
  if (mi.durationSeconds && mi.durationSeconds > 0) {
    rows.push(['Run Time', formatMediaRunTime(mi.durationSeconds)]);
  } else {
    rows.push(['Run Time', '—']);
  }
  rows.push(['Scan Type', formatScanType(mi.scanType)]);
  rows.push(['Subtitles', subtitleLanguagesDisplay(subs)]);
  return rows;
}

// renderMediaInfoBody returns the bare label/value rows for an inline
// Media Info expansion. Used by the per-version disclosure sub-row in
// the Files section. Empty mi yields a single placeholder line so the
// expanded sub-row never renders blank.
function renderMediaInfoBody(mi) {
  const rows = mediaInfoRows(mi);
  if (rows.length === 0) {
    return m('div', { class: 'opacity-60 text-sm py-2' },
      'No media info captured for this version.');
  }
  return m('dl', {
    class: 'grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-1 py-2',
  },
    rows.flatMap(([label, value]) => [
      m('dt', { class: 'opacity-60 text-sm' }, label),
      m('dd', { class: 'font-mono text-sm break-all' }, value || '—'),
    ]));
}

// renderNFORow is the bottom-of-Files-section disclosure for the
// recording's movie.nfo. Collapsed by default with mtime; click to
// expand into a syntax-highlighted XML pane. The "no NFO yet" branch
// renders a non-expandable placeholder so the SPA always surfaces the
// file's status.
//
// Highlighting prefers window.hljs (loaded via the SPA shell). When
// hljs isn't on the page we fall back to a plain <pre> — the
// rendering is still readable, just not coloured.
function renderNFORow(loaded) {
  const content = loaded.nfo_content || '';
  if (!content) {
    return m('div', { class: 'flex items-center gap-2 py-2 text-sm' }, [
      m('span', { class: 'font-mono opacity-80' }, 'movie.nfo'),
      m('span', { class: 'opacity-60' }, '· not yet written'),
    ]);
  }
  const rel = relativeTime(loaded.nfo_modified_at);
  const note = (rel && rel !== '—') ? 'modified ' + rel : 'on disk';
  const expanded = !!state.recording.nfoExpanded;
  return m('div', { class: 'space-y-2' }, [
    m('div', { class: 'flex items-center justify-between gap-3 py-2' }, [
      m('div', { class: 'flex items-center gap-2 text-sm min-w-0' }, [
        m('span', { class: 'font-mono opacity-80' }, 'movie.nfo'),
        m('span', { class: 'opacity-60 truncate' }, '· ' + note),
      ]),
      m('button', {
        type: 'button',
        class: 'btn btn-xs btn-ghost',
        'aria-expanded': expanded ? 'true' : 'false',
        onclick: () => {
          state.recording.nfoExpanded = !expanded;
        },
      }, expanded ? 'Hide' : 'Show'),
    ]),
    expanded ? m(NFOBody, { content }) : null,
  ]);
}

// NFOBody renders the highlighted XML pane. Plain content goes into
// the DOM via Mithril, then oncreate / onupdate calls hljs.highlight
// Element on the rendered <code> — no m.trust, no race with hljs's
// load timing. When hljs isn't available the pane stays plain;
// readable enough.
const NFOBody = {
  oncreate(vnode) { applyHighlight(vnode); },
  onupdate(vnode) { applyHighlight(vnode); },
  view(vnode) {
    const content = (vnode.attrs && vnode.attrs.content) || '';
    return m('pre', {
      class: 'hljs bg-base-200 text-xs p-3 rounded overflow-x-auto whitespace-pre',
    }, m('code', { class: 'language-xml' }, content));
  },
};

// applyHighlight runs hljs.highlightElement against the <code> child
// of the supplied Mithril vnode. The data-highlighted attribute reset
// is required so re-applying highlight after content changes (NFO
// rewrite, recording id swap) actually re-tokenizes the new text —
// hljs short-circuits when it sees the marker.
function applyHighlight(vnode) {
  if (typeof window === 'undefined' || !window.hljs) return;
  const code = vnode.dom && vnode.dom.querySelector('code');
  if (!code) return;
  if (code.dataset) delete code.dataset.highlighted;
  code.removeAttribute('data-highlighted');
  try {
    window.hljs.highlightElement(code);
  } catch (_) {
    // If hljs throws (unregistered language, malformed input, etc.)
    // leave the plain text in place.
  }
}

// loadOptions fetches /api/v1/<entity>/<id>/<kind>-options and parks
// the result on state.recording.pickerOptions[kind]. Errors land on
// pickerOptionsError[kind] so the picker tab can render an inline
// alert. Loading flips pickerOptionsLoading[kind] for the duration.
//
// opts.refresh appends ?refresh=true so the fanart fallback's frame
// cache gets scrubbed before re-extracting — Re-fetch on the Fanart
// tab uses this to re-roll the random frame offsets.
function loadOptions(id, kind, opts) {
  state.recording.pickerOptionsLoading = state.recording.pickerOptionsLoading || {};
  state.recording.pickerOptions = state.recording.pickerOptions || {};
  state.recording.pickerOptionsError = state.recording.pickerOptionsError || {};
  state.recording.pickerOptionsGen = state.recording.pickerOptionsGen || {};
  state.recording.pickerOptionsLoading[kind] = true;
  state.recording.pickerOptionsError[kind] = null;
  // Don't clear pickerOptions on refetch — the user prefers seeing the
  // stale strip while the new fetch flies than a flash to skeletons.
  m.redraw();

  const refresh = opts && opts.refresh;
  const path = '/recordings/' + encodeURIComponent(id) + '/' + kind + '-options' +
    (refresh ? '?refresh=true' : '');
  return api.get(path)
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
  // Frame-fallback caption: when the fanart options came back with
  // source="frames" the server is showing locally-extracted stills,
  // not curated Encora screenshots. Surfacing this so the user
  // doesn't think they're picking from upstream art.
  const optionsList = optionsByKind[kind];
  const isFrameFallback = kind === 'fanart' &&
    Array.isArray(optionsList) && optionsList.length > 0 &&
    optionsList.every((o) => o && o.source === 'frames');
  const optionsCaption = isFrameFallback
    ? 'Random frames from your local file. Click Re-fetch for a different set.'
    : null;

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
    onRefetch: () => loadOptions(id, kind, { refresh: kind === 'fanart' }),
    uploadLabel: kind === 'fanart' ? 'Upload fanart' : 'Upload poster',
    optionsCaption,
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

// renderBody composes the post-hero stack. Phase 2 collapses the
// trio of Local versions / Media Info / NFO output cards into one
// Files section with per-row inline disclosures, adds a Details
// section above it for the metadata fields the hero dropped
// (Cataloged / Gifting / Owners / Wanters / Folder), and keeps the
// Cast section + NFT callout untouched.
function renderBody(loaded) {
  const callout = renderNFTCallout(loaded);
  return m('div', { class: 'space-y-6' }, [
    callout,
    renderDetailsSection(loaded),
    renderFilesSection(loaded),
    renderCastCard(loaded),
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
    state.recording.renameOpen = false;
    state.recording.renamePreview = null;
    state.recording.renamePreviewLoading = false;
    state.recording.renamePreviewError = null;
    state.recording.renameApplying = false;
    state.recording.renameResult = null;
    state.recording.renameApplyError = null;
    state.recording.regeneratingNFO = false;
    state.recording.regenerateNFOMessage = null;
    state.recording.regenerateNFOError = null;
    state.recording.pickerStaged = {};
    // Phase 2 disclosures: per-version Media Info expansion + the
    // NFO row's Show/Hide toggle. Reset on every recording switch so
    // a previously-open expansion doesn't bleed onto a new row.
    state.recording.expandedVersions = {};
    state.recording.nfoExpanded = false;
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
      state.recording.renameOpen = false;
      state.recording.renamePreview = null;
      state.recording.renamePreviewLoading = false;
      state.recording.renamePreviewError = null;
      state.recording.renameApplying = false;
      state.recording.renameResult = null;
      state.recording.renameApplyError = null;
      state.recording.regeneratingNFO = false;
      state.recording.regenerateNFOMessage = null;
      state.recording.regenerateNFOError = null;
      state.recording.expandedVersions = {};
      state.recording.nfoExpanded = false;
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
      renderImagePickerModal(loaded),
      m(RecordingRenameModal, {
        open: !!state.recording.renameOpen,
        recordingID: loaded.Recording.id,
        onClose: () => {
          state.recording.renameOpen = false;
        },
        onApplied: () => {
          // Re-fetch the recording detail after a successful apply so
          // the Local versions card reflects the new file paths and
          // the NFO card picks up the post-rename rewrite.
          loadRecording(loaded.Recording.id);
        },
      }),
      renderImageErrorToast({
        error: state.recording.imageError,
        onDismiss: () => { state.recording.imageError = null; },
      }),
      renderImageInfoToast({
        message: state.recording.imageInfo,
        onDismiss: () => { state.recording.imageInfo = null; },
      }),
      // Regenerate-NFO confirmation / error toasts — re-use the same
      // alert-pair pattern as the image surfaces above so the user
      // sees a single visual style across all detail-page outcomes.
      renderImageInfoToast({
        message: state.recording.regenerateNFOMessage,
        onDismiss: () => { state.recording.regenerateNFOMessage = null; },
      }),
      renderImageErrorToast({
        error: state.recording.regenerateNFOError,
        onDismiss: () => { state.recording.regenerateNFOError = null; },
      }),
    ]);
  },
};

export default Recording;
