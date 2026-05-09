// uploadPicker.js — small shared helper for the user image-upload
// flow. Owned by Recording.js today; the show-detail agent picks this
// up once their work merges so the upload-button rendering logic
// stays in one place rather than duplicated across two components.
//
// The helper does the file-picker dance + multipart POST + error
// surfacing, but does NOT update component-local state. Callers pass
// onUploaded(index) to react to a successful upload (typically: refresh
// the picker payload + post the matching selection endpoint so the
// upload becomes the active choice). Errors are returned via
// onError(message) so the parent component can park them on its own
// imageError-style state field.

import m from 'https://esm.sh/mithril@2.2.2';

// uploadFile POSTs a single File / Blob to the supplied API path as
// multipart/form-data. Returns a promise that resolves with the
// response body on success, rejects on a network or non-2xx error
// (with a normalized Error carrying .status + .body when available).
//
// Path is server-relative and must already include /api/v1; this
// helper bypasses the api.js wrapper because m.request only accepts
// FormData when serialize is overridden, and multipart bodies don't
// belong on the JSON helper anyway.
export function uploadFile(path, file) {
  const fd = new FormData();
  fd.append('file', file);
  return m.request({
    url: path,
    method: 'POST',
    body: fd,
    // serialize: identity — m.request would otherwise JSON.stringify
    // the FormData and produce "{}".
    serialize: (x) => x,
  }).catch((reason) => {
    const err = new Error(
      'HTTP ' + (reason && reason.code ? reason.code : 'error') +
      ' on ' + path,
    );
    if (reason && reason.code != null) err.status = reason.code;
    if (reason && reason.response != null) err.body = reason.response;
    if (reason && reason.message) err.message = reason.message;
    throw err;
  });
}

// errorMessageFromUpload extracts a human-readable string from a
// thrown upload error. Tries the JSON {error: "..."} field first,
// then falls back to the raw body text or the Error.message. Mirrors
// errorMessage() in Recording.js but stays decoupled so the helper
// can be used from any component.
export function errorMessageFromUpload(err) {
  if (!err) return 'unknown error';
  if (err.body) {
    try {
      const parsed = JSON.parse(err.body);
      if (parsed && parsed.error) return String(parsed.error);
      if (parsed && parsed.message) return String(parsed.message);
    } catch (_) { /* non-JSON body; fall through. */ }
    if (typeof err.body === 'string' && err.body.length < 240) {
      return err.body;
    }
  }
  return err.message || String(err);
}

// renderUploadButton returns a Mithril vnode that, when clicked,
// opens a hidden <input type="file"> and pipes the selection through
// onSelect(file). Used by the recording + show poster/backdrop strips
// so each thumbnail strip can sit next to a small "Upload" affordance.
//
// opts:
//   label    — button text (default "Upload")
//   accept   — input accept attribute (default "image/*")
//   disabled — disables the button when truthy
//   onSelect — required, fired with (File) when the user picks one
export function renderUploadButton(opts) {
  const label = opts.label || 'Upload';
  const accept = opts.accept || 'image/*';
  const disabled = !!opts.disabled;
  const onSelect = opts.onSelect;
  // The file input is hidden; the visible button proxies the click.
  // file-input from DaisyUI is too prominent for a per-strip control,
  // so we render a btn-sm and trigger a hidden <input> ref.
  let inputEl = null;
  return m('div', { class: 'inline-flex items-center' }, [
    m('input', {
      type: 'file',
      accept,
      class: 'hidden',
      oncreate: (vn) => { inputEl = vn.dom; },
      onchange: (ev) => {
        const f = ev.target.files && ev.target.files[0];
        // Reset the input value so picking the same file twice in a
        // row still fires onchange the second time.
        ev.target.value = '';
        if (f && typeof onSelect === 'function') onSelect(f);
      },
    }),
    m('button', {
      type: 'button',
      class: 'btn btn-sm btn-ghost',
      disabled,
      onclick: () => {
        if (inputEl) inputEl.click();
      },
    }, label),
  ]);
}

export default {
  uploadFile,
  errorMessageFromUpload,
  renderUploadButton,
};
