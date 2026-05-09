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
