// RecordingRenameModal.js — DaisyUI <dialog>-based modal that drives
// the per-recording rename preview + apply flow.
//
// Lifecycle:
//   1. Parent (Recording.js) flips state.recording.renameOpen=true and
//      kicks loadRenamePreview() to fetch the per-version source →
//      destination plan via the previewRecordingRename query.
//   2. While the preview is loading the body shows a spinner. On
//      success it shows one row per version with monospace From / To
//      paths plus a "Will move" / "Already canonical" status. Per-row
//      errors render inline so a partial preview is still readable.
//   3. Footer: Cancel + Apply. Apply is disabled when no row has
//      willMove=true (every version is already canonical) and while
//      the apply mutation is in flight.
//   4. Clicking Apply fires applyRecordingRename. The body switches
//      to a result view (one row per version showing moved / failed)
//      and the footer's Apply button morphs into Done. Done closes
//      the modal AND triggers the parent's onClosed callback so the
//      recording detail data refetches with the new file paths.
//
// Wire format: the recordingID stays the prefixed "recording-N" form
// the rest of the SPA uses; per-row versionID stays "version-N".

import m from 'https://esm.sh/mithril@2.2.2';
import graphql from '../graphql.js';
import state from '../state.js';

// PREVIEW_QUERY pulls the per-version source / destination plan. Empty
// list when the recording has no versions; per-row error fields
// surface plan failures (probe or template errors) without aborting
// the batch.
const PREVIEW_QUERY = `
  query PreviewRecordingRename($id: ID!) {
    previewRecordingRename(recordingID: $id) {
      versionID
      source
      destination
      willMove
      error
    }
  }
`;

// APPLY_MUTATION moves each version's source file to its canonical
// destination, persists the new file_path, and (on full success) fans
// an NFO rewrite to the new folder. Per-version failures populate the
// row's error field but do not abort the batch.
const APPLY_MUTATION = `
  mutation ApplyRecordingRename($id: ID!) {
    applyRecordingRename(recordingID: $id) {
      versionID
      source
      destination
      moved
      error
    }
  }
`;

// loadRenamePreview fires the previewRecordingRename query for the
// recording id. The result lands on state.recording.renamePreview.
// recordingID is the bare int the page route carries; we prefix on
// the way out to GraphQL.
export function loadRenamePreview(recordingID) {
  state.recording.renamePreviewLoading = true;
  state.recording.renamePreviewError = null;
  state.recording.renamePreview = null;
  state.recording.renameResult = null;
  state.recording.renameApplyError = null;
  m.redraw();

  return graphql.query(PREVIEW_QUERY, { id: 'recording-' + recordingID })
    .then((data) => {
      state.recording.renamePreview =
        (data && data.previewRecordingRename) || [];
      state.recording.renamePreviewLoading = false;
      m.redraw();
    })
    .catch((err) => {
      state.recording.renamePreviewLoading = false;
      state.recording.renamePreviewError =
        (err && err.message) || 'Could not load rename preview.';
      m.redraw();
    });
}

// runApply fires the applyRecordingRename mutation for the recording
// id. The result lands on state.recording.renameResult so the modal
// body switches from the preview view to the per-version outcome
// view. The modal footer's Apply button becomes Done; clicking it
// calls onClosed so the parent re-fetches the recording detail.
function runApply(recordingID) {
  state.recording.renameApplying = true;
  state.recording.renameApplyError = null;
  m.redraw();

  return graphql.query(APPLY_MUTATION, { id: 'recording-' + recordingID })
    .then((data) => {
      state.recording.renameResult =
        (data && data.applyRecordingRename) || [];
      state.recording.renameApplying = false;
      m.redraw();
    })
    .catch((err) => {
      state.recording.renameApplying = false;
      state.recording.renameApplyError =
        (err && err.message) || 'Apply failed.';
      m.redraw();
    });
}

// renderPreviewRow draws one row of the preview body: Version label,
// monospace From + To paths, and the willMove / canonical / error
// status line.
function renderPreviewRow(item, index) {
  const status = item.error
    ? m('div', { class: 'text-sm text-error' }, 'Error: ' + item.error)
    : (item.willMove
        ? m('div', { class: 'text-sm text-success' }, 'Will move')
        : m('div', { class: 'text-sm opacity-60' }, 'Already canonical'));
  return m('div', {
    key: item.versionID,
    class: 'space-y-1 p-3 rounded-box bg-base-200',
  }, [
    m('div', { class: 'text-xs uppercase opacity-60 tracking-wide' },
      'Version ' + (index + 1)),
    m('div', { class: 'space-y-0.5' }, [
      m('div', { class: 'flex gap-2 text-xs font-mono' }, [
        m('span', { class: 'opacity-60 shrink-0' }, 'From:'),
        m('span', { class: 'break-all', title: item.source }, item.source || '—'),
      ]),
      m('div', { class: 'flex gap-2 text-xs font-mono' }, [
        m('span', { class: 'opacity-60 shrink-0' }, 'To:'),
        m('span', { class: 'break-all', title: item.destination },
          item.destination || '—'),
      ]),
    ]),
    status,
  ]);
}

// renderResultRow draws one row of the post-apply body. Mirrors
// renderPreviewRow's layout but the status string differs (moved /
// already canonical / failed).
function renderResultRow(item, index) {
  let status;
  if (item.error) {
    status = m('div', { class: 'text-sm text-error' },
      'Failed: ' + item.error);
  } else if (item.moved) {
    status = m('div', { class: 'text-sm text-success' }, 'Moved');
  } else {
    status = m('div', { class: 'text-sm opacity-60' }, 'Already canonical');
  }
  return m('div', {
    key: item.versionID,
    class: 'space-y-1 p-3 rounded-box bg-base-200',
  }, [
    m('div', { class: 'text-xs uppercase opacity-60 tracking-wide' },
      'Version ' + (index + 1)),
    m('div', { class: 'space-y-0.5' }, [
      m('div', { class: 'flex gap-2 text-xs font-mono' }, [
        m('span', { class: 'opacity-60 shrink-0' }, 'From:'),
        m('span', { class: 'break-all', title: item.source }, item.source || '—'),
      ]),
      m('div', { class: 'flex gap-2 text-xs font-mono' }, [
        m('span', { class: 'opacity-60 shrink-0' }, 'To:'),
        m('span', { class: 'break-all', title: item.destination },
          item.destination || '—'),
      ]),
    ]),
    status,
  ]);
}

// renderBody chooses between the preview view and the result view
// based on whether the apply mutation has landed yet.
function renderBody() {
  const r = state.recording;
  if (r.renameResult) {
    if (!r.renameResult.length) {
      return m('div', { class: 'opacity-60 text-sm italic' },
        'No versions to apply.');
    }
    return m('div', { class: 'space-y-3' }, r.renameResult.map(renderResultRow));
  }
  if (r.renamePreviewLoading) {
    return m('div', { class: 'flex items-center gap-2 text-sm opacity-70' }, [
      m('span', { class: 'loading loading-spinner loading-xs' }),
      'Computing preview…',
    ]);
  }
  if (r.renamePreviewError) {
    return m('div', { role: 'alert', class: 'alert alert-error text-sm' },
      m('span', 'Could not compute preview: ' + r.renamePreviewError));
  }
  const items = r.renamePreview || [];
  if (!items.length) {
    return m('div', { class: 'opacity-60 text-sm italic' },
      'No versions on disk for this recording.');
  }
  return m('div', { class: 'space-y-3' }, items.map(renderPreviewRow));
}

// hasMovableRow reports whether the preview includes at least one row
// whose willMove flag is true. The Apply button keys off this so the
// user can't fire the mutation when every version is already
// canonical (the mutation would still succeed as a no-op, but the UX
// reads better with the disabled state).
function hasMovableRow(items) {
  if (!Array.isArray(items)) return false;
  return items.some((it) => it && it.willMove === true);
}

// RecordingRenameModal mirrors QueueImportModal's <dialog> lifecycle —
// showModal()/close() driven off attrs.open. The native 'close' event
// (Esc, backdrop click, ✕) forwards to attrs.onClose so the parent
// component flips state.recording.renameOpen back to false.
const RecordingRenameModal = {
  oncreate(vnode) {
    const dom = vnode.dom;
    dom.addEventListener('close', () => {
      if (typeof vnode.attrs.onClose === 'function') {
        vnode.attrs.onClose();
        m.redraw();
      }
    });
    if (vnode.attrs.open && !dom.open) dom.showModal();
  },

  onupdate(vnode) {
    const dom = vnode.dom;
    if (vnode.attrs.open && !dom.open) dom.showModal();
    else if (!vnode.attrs.open && dom.open) dom.close();
  },

  view(vnode) {
    const a = vnode.attrs;
    if (!a.open) {
      // Render an empty <dialog> when closed so the browser-managed
      // open state doesn't fight a stale render.
      return m('dialog', { class: 'modal' },
        m('div', { class: 'modal-box max-w-3xl' }, ' '));
    }
    const r = state.recording;
    const items = r.renamePreview || [];
    const showResult = !!r.renameResult;
    const canApply = !showResult
      && !r.renameApplying
      && hasMovableRow(items);

    let primaryAction;
    if (showResult) {
      primaryAction = m('button', {
        type: 'button',
        class: 'btn btn-primary btn-sm',
        onclick: () => {
          if (typeof a.onClose === 'function') a.onClose();
          if (typeof a.onApplied === 'function') a.onApplied();
        },
      }, 'Done');
    } else {
      primaryAction = m('button', {
        type: 'button',
        class: 'btn btn-primary btn-sm',
        disabled: !canApply,
        onclick: () => runApply(a.recordingID),
      }, [
        r.renameApplying
          ? m('span', { class: 'loading loading-spinner loading-xs mr-1' })
          : null,
        r.renameApplying ? 'Applying…' : 'Apply rename',
      ]);
    }

    return m('dialog', { class: 'modal' }, [
      m('div', { class: 'modal-box max-w-3xl' }, [
        m('form', { method: 'dialog' },
          m('button', {
            type: 'submit',
            class: 'btn btn-sm btn-circle btn-ghost absolute right-2 top-2',
            'aria-label': 'Close',
          }, '✕')),
        m('h3', { class: 'font-bold text-lg mb-2 pr-8' },
          showResult ? 'Rename results' : 'Preview rename'),
        m('p', { class: 'text-sm opacity-70 mb-4' }, showResult
          ? 'Per-version outcomes. Successful moves persisted to the ' +
            'database; failed rows kept their original location.'
          : 'Per-version source → destination under the current ' +
            'rename templates. Apply moves the files and rewrites the ' +
            'NFO; the source folder is left in place.'),
        m('div', { class: 'mb-4' }, renderBody()),
        r.renameApplyError
          ? m('div', { role: 'alert', class: 'alert alert-error text-sm mb-2' },
              m('span', 'Apply failed: ' + r.renameApplyError))
          : null,
        m('div', { class: 'modal-action' }, [
          showResult
            ? null
            : m('button', {
                type: 'button',
                class: 'btn btn-ghost btn-sm',
                disabled: r.renameApplying,
                onclick: () => {
                  if (typeof a.onClose === 'function') a.onClose();
                },
              }, 'Cancel'),
          primaryAction,
        ]),
      ]),
      m('form', { method: 'dialog', class: 'modal-backdrop' },
        m('button', { type: 'submit', 'aria-label': 'Close' }, 'close')),
    ]);
  },
};

export default RecordingRenameModal;
