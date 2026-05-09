// ImagePickerModal.js — DaisyUI <dialog>-based modal that hosts the
// image-picker subsections previously rendered as a giant inline card
// at the top of the recording / show detail page.
//
// Two modes:
//   kind: 'recording'  — three tabs (poster / backdrop / overlay text).
//                        Each tab renders the per-section vnode from
//                        the parent (passed in via attrs.tabs).
//   kind: 'show'       — single body, just the poster picker. No tabs.
//
// The component is "dumb" about state — the parent owns the open flag,
// the active-tab pointer, the busy/error fields, and the renderers
// for each section. We only do three things:
//   1. Sync the <dialog> open state to attrs.open via showModal()/close()
//      in oncreate + onupdate (so toggling the parent's flag reflects
//      visually, including Esc-to-close which dispatches a 'close' event
//      we forward via attrs.onClose).
//   2. Render the title row + close button + (optional) tab strip.
//   3. Render the active section's vnode in the modal body.
//
// The image error toast is rendered by the parent — outside the modal
// so users see upload/save failures even after the modal closes.

import m from 'https://esm.sh/mithril@2.2.2';

// PencilIcon is the small pencil-on-square glyph used for the
// "Edit images" header button. Inlined as a Mithril vnode so the
// component file stays self-contained.
export function PencilIcon() {
  return m('svg', {
    xmlns: 'http://www.w3.org/2000/svg', width: 16, height: 16,
    viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'aria-hidden': 'true', class: 'h-4 w-4 shrink-0',
  }, [
    m('path', { d: 'M12 20h9' }),
    m('path', {
      d: 'M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4 12.5-12.5z',
    }),
  ]);
}

// renderEditImagesButton is the small ghost-styled action that opens
// the modal. Used by both Recording.js and Show.js so the affordance
// looks identical across the two pages.
export function renderEditImagesButton({ onclick, disabled }) {
  return m('button', {
    type: 'button',
    class: 'btn btn-sm btn-ghost gap-2',
    disabled: !!disabled,
    onclick,
  }, [
    PencilIcon(),
    m('span', 'Edit images'),
  ]);
}

// renderImageInfoToast surfaces a transient success/info message
// (e.g. "Refresh queued — page will update when complete.") as a
// DaisyUI alert-info toast pinned to the bottom-right. Same dismiss
// pattern as renderImageErrorToast — onDismiss clears the parent's
// state field. Auto-stays until the parent clears it; the parent is
// expected to setTimeout the dismiss for short-lived confirmations.
export function renderImageInfoToast({ message, onDismiss }) {
  if (!message) return null;
  return m('div', { class: 'toast toast-end z-50' }, [
    m('div', { role: 'status', class: 'alert alert-info' }, [
      m('span', { class: 'text-sm' }, message),
      m('button', {
        type: 'button',
        class: 'btn btn-xs btn-ghost',
        'aria-label': 'Dismiss',
        onclick: onDismiss,
      }, '×'),
    ]),
  ]);
}

// renderImageErrorToast surfaces state.recording.imageError /
// state.show.imageError as a DaisyUI toast pinned to the bottom-right
// of the viewport. Sits outside the modal so a failed POST still
// reaches the user even if they closed the dialog mid-action.
//
// onDismiss clears the parent's error field so the toast can be
// dismissed by hand (the alert auto-stays until cleared — there is no
// auto-timeout because failure messages benefit from explicit ack).
export function renderImageErrorToast({ error, onDismiss }) {
  if (!error) return null;
  return m('div', { class: 'toast toast-end z-50' }, [
    m('div', { role: 'alert', class: 'alert alert-error' }, [
      m('span', { class: 'text-sm' }, error),
      m('button', {
        type: 'button',
        class: 'btn btn-xs btn-ghost',
        'aria-label': 'Dismiss',
        onclick: onDismiss,
      }, '×'),
    ]),
  ]);
}

// renderUpstreamPicker is the shared picker tab body used by both
// Recording.js (poster + fanart) and Show.js (banner). It renders:
//   1. The current local image (the chosen slot under v2).
//   2. A live-fetched strip of upstream thumbnails. Click one ->
//      POST the chosen URL into the slot.
//   3. An Upload button for "the upstream options aren't what I want"
//      override.
//   4. A "Re-fetch upstream options" footer button so the user can
//      re-pull the strip without closing the modal — useful right
//      after firing the refresh-images job.
//
// Attrs:
//   currentURL    — /images/... path of the current local image (or "").
//   currentLabel  — caption under the current image ("Current poster").
//   currentAlt    — alt text for the current image.
//   aspect        — 'poster' | 'fanart' | 'banner' for thumbnail sizing.
//   options       — array of {url, source} from the options endpoint.
//                   null/undefined while the fetch is in flight; an
//                   empty array means "no upstream options found".
//   loading       — true while the options fetch is in flight.
//   error         — string error from the options fetch (e.g. 503).
//   busy          — disables every action button while a POST runs.
//   onPick(url)   — fired when the user clicks an upstream thumbnail.
//   onUpload(file)— fired when the user picks a file via Upload.
//   onRefetch()   — fired when the user hits "Re-fetch upstream options".
//   uploadLabel   — Upload button label ("Upload poster", etc.).
//
// Skeleton + error + empty states all match DaisyUI conventions
// (skeleton-rectangle, alert-error, neutral muted text).
export function renderUpstreamPicker(attrs) {
  const {
    currentURL, currentLabel, currentAlt,
    aspect, options, loading, error, busy,
    onPick, onUpload, onRefetch, uploadLabel,
  } = attrs;

  // Aspect drives both the thumbnail and the "current" preview tile.
  const thumbClass = aspect === 'fanart' || aspect === 'backdrop'
    ? 'aspect-video w-48'
    : 'aspect-[2/3] w-32';
  const currentClass = aspect === 'fanart' || aspect === 'backdrop'
    ? 'aspect-video w-full max-w-2xl'
    : 'aspect-[2/3] w-48';

  return m('section', { class: 'space-y-4' }, [
    // Current local image — single slot under v2.
    m('div', { class: 'space-y-2' }, [
      m('h3', { class: 'text-sm font-semibold' }, currentLabel),
      currentURL
        ? m('figure', {
            class: 'relative rounded overflow-hidden bg-base-200 ' + currentClass,
          }, [
            m('img', {
              src: currentURL,
              alt: currentAlt || '',
              class: 'w-full h-full object-cover',
              loading: 'lazy',
            }),
            m('span', {
              class: 'absolute top-2 left-2 badge badge-primary badge-sm',
            }, 'Current'),
          ])
        : m('div', {
            class: 'rounded bg-base-200 flex items-center justify-center ' +
                   'text-base-content/40 text-sm font-mono ' + currentClass,
          }, 'No image on disk yet'),
    ]),

    // Upstream options strip.
    m('div', { class: 'space-y-2' }, [
      m('div', { class: 'flex items-center justify-between gap-2 flex-wrap' }, [
        m('h3', { class: 'text-sm font-semibold' }, 'Upstream options'),
        m('div', { class: 'flex items-center gap-2' }, [
          uploadButton({
            label: uploadLabel || 'Upload',
            disabled: busy,
            onSelect: onUpload,
          }),
          m('button', {
            type: 'button',
            class: 'btn btn-sm btn-ghost gap-1',
            disabled: busy || loading,
            onclick: onRefetch,
          }, [
            loading
              ? m('span', { class: 'loading loading-spinner loading-xs' })
              : null,
            'Re-fetch',
          ]),
        ]),
      ]),
      renderUpstreamStrip({
        options, loading, error, busy, onPick, thumbClass, onRefetch,
      }),
    ]),
  ]);
}

// renderUpstreamStrip renders the thumbnail row inside the picker
// body: skeleton tiles while loading, an error alert on failure,
// an empty-state message when upstream had nothing, otherwise a
// horizontally-scrollable strip of clickable thumbnails.
function renderUpstreamStrip(attrs) {
  const { options, loading, error, busy, onPick, thumbClass } = attrs;
  if (loading && !Array.isArray(options)) {
    // Skeleton placeholders — three rectangles matching the picker's
    // expected aspect so the layout doesn't jump on resolution.
    return m('div', { class: 'flex gap-3 overflow-x-auto py-2' },
      [0, 1, 2].map((i) => m('div', {
        key: 'sk-' + i,
        class: 'skeleton ' + thumbClass + ' shrink-0',
      })));
  }
  if (error) {
    return m('div', { role: 'alert', class: 'alert alert-error' }, [
      m('span', { class: 'text-sm' }, error),
      m('button', {
        type: 'button',
        class: 'btn btn-sm btn-ghost',
        disabled: busy,
        onclick: attrs.onRefetch,
      }, 'Try again'),
    ]);
  }
  if (!Array.isArray(options) || options.length === 0) {
    return m('div', { class: 'opacity-60 text-sm py-2' },
      'No upstream options found. Upload a custom image instead.');
  }
  return m('div', { class: 'flex gap-3 overflow-x-auto py-2' },
    options.map((opt, idx) => m('button', {
      key: opt.url + '-' + idx,
      type: 'button',
      class: 'shrink-0 rounded overflow-hidden bg-base-200 ' +
             'border-2 border-transparent hover:border-primary ' +
             'focus:outline-none focus:border-primary ' +
             'disabled:opacity-50 disabled:cursor-not-allowed ' +
             thumbClass,
      title: opt.source ? 'from ' + opt.source : '',
      disabled: busy,
      onclick: () => onPick(opt.url),
    }, m('img', {
      src: opt.url,
      alt: opt.source || 'upstream option ' + (idx + 1),
      class: 'w-full h-full object-cover',
      loading: 'lazy',
    }))));
}

// uploadButton mirrors the renderUploadButton helper from
// utils/uploadPicker.js but is inlined here to avoid coupling the
// modal to the helper's exact interface. The picker passes the file
// straight through to onUpload(file).
function uploadButton(opts) {
  let inputEl = null;
  return m('div', { class: 'inline-flex items-center' }, [
    m('input', {
      type: 'file',
      accept: 'image/*',
      class: 'hidden',
      oncreate: (vn) => { inputEl = vn.dom; },
      onchange: (ev) => {
        const f = ev.target.files && ev.target.files[0];
        ev.target.value = '';
        if (f && typeof opts.onSelect === 'function') opts.onSelect(f);
      },
    }),
    m('button', {
      type: 'button',
      class: 'btn btn-sm btn-ghost',
      disabled: !!opts.disabled,
      onclick: () => { if (inputEl) inputEl.click(); },
    }, opts.label || 'Upload'),
  ]);
}

// ImagePickerModal renders the <dialog>. Attrs:
//   open      — boolean; mirrors state.X.pickerOpen.
//   onClose   — fired when the dialog closes (Esc, backdrop, ✕).
//   title     — header title string. Default "Edit images".
//   tabs      — optional [{key, label, render}]. When present, the
//               modal renders a tab strip and switches the body based
//               on activeTab. When absent the body is a single render().
//   activeTab — current tab key (only used when tabs is set).
//   onTabChange — fired when the user clicks a tab.
//   render    — () => vnode; used when tabs is unset (show mode).
//   busy      — surfaces the spinner in the title row.
//   footerActions — optional [{label, onClick, disabled, primary}].
//                   Rendered as a row of buttons at the bottom of the
//                   modal body, e.g. the "Refresh from upstream" job
//                   trigger that closes the modal + queues a toast.
const ImagePickerModal = {
  oncreate(vnode) {
    const dom = vnode.dom;
    // Forward the native 'close' event (Esc, form method=dialog
    // submit, backdrop click) so the parent can flip its open flag.
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
    const title = a.title || 'Edit images';
    const tabs = Array.isArray(a.tabs) ? a.tabs : null;
    const activeTab = a.activeTab;
    const busy = !!a.busy;

    // Body switches between tabs[].render() and a.render() depending
    // on whether the parent supplied a tab list. The active vnode is
    // resolved fresh each render so component state stays live.
    let body = null;
    if (tabs) {
      const active = tabs.find((t) => t.key === activeTab) || tabs[0];
      body = active && typeof active.render === 'function'
        ? active.render() : null;
    } else if (typeof a.render === 'function') {
      body = a.render();
    }

    return m('dialog', { class: 'modal' }, [
      m('div', { class: 'modal-box max-w-4xl' }, [
        // Close button at the corner. method=dialog so the click
        // closes the dialog without us needing to wire onclick.
        m('form', { method: 'dialog' },
          m('button', {
            type: 'submit',
            class: 'btn btn-sm btn-circle btn-ghost absolute right-2 top-2',
            'aria-label': 'Close',
          }, '✕')),
        m('div', { class: 'flex items-center gap-3 mb-3 pr-8' }, [
          m('h3', { class: 'font-bold text-lg' }, title),
          busy
            ? m('span', { class: 'loading loading-spinner loading-sm' })
            : null,
        ]),
        tabs
          ? m('div', { role: 'tablist', class: 'tabs tabs-box mb-4' },
              tabs.map((t) => m('a', {
                role: 'tab',
                class: 'tab' + (t.key === activeTab ? ' tab-active' : ''),
                onclick: (ev) => {
                  ev.preventDefault();
                  if (typeof a.onTabChange === 'function') a.onTabChange(t.key);
                },
              }, t.label)))
          : null,
        m('div', { class: 'space-y-2' }, body),
        Array.isArray(a.footerActions) && a.footerActions.length > 0
          ? m('div', { class: 'modal-action mt-4' },
              a.footerActions.map((act, idx) => m('button', {
                key: 'fa-' + idx,
                type: 'button',
                class: 'btn btn-sm ' + (act.primary ? 'btn-primary' : 'btn-ghost'),
                disabled: !!act.disabled,
                onclick: act.onClick,
              }, act.label)))
          : null,
      ]),
      // Backdrop form — clicking outside the modal-box submits and
      // closes the dialog (DaisyUI 5 idiom). The native 'close' event
      // fires next, which we forwarded in oncreate, so the parent's
      // open flag flips back to false.
      m('form', { method: 'dialog', class: 'modal-backdrop' },
        m('button', { type: 'submit', 'aria-label': 'Close' }, 'close')),
    ]);
  },
};

export default ImagePickerModal;
