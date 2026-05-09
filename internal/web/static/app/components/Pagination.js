// Pagination.js — shared Prev / Next + page indicator strip used by
// every list page (Library, People, Wants, History).
//
// Owns no internal state. Caller passes (offset, limit, total) and a
// setOffset(newOffset) callback; the component re-derives the current
// page number, hides itself when there's only one page, and disables
// Prev / Next when at either end. Clicking Prev or Next also smooth-
// scrolls the window to the top so the next page lands at the user's
// eye-line — small UX win over a manual scroll-up.
//
// Visual: DaisyUI `join` group with btn-sm Prev / page-indicator /
// Next buttons. Caller can flip showRange=true to render
// "Showing 51–100 of 1,234" instead of the default "Page 3 of 25" —
// useful for the History page where the user thinks in events, not
// pages, but every other list page reads better as page numbers.

import m from 'https://esm.sh/mithril@2.2.2';

// scrollToTop is the small UX flourish the spec calls out — when the
// user clicks Prev / Next, jump back to the top of the list so the
// next page starts at eye-level rather than mid-scroll. The behavior:
// 'smooth' option is widely supported and degrades gracefully on
// browsers without it.
function scrollToTop() {
  if (typeof window === 'undefined' || !window.scrollTo) return;
  window.scrollTo({ top: 0, behavior: 'smooth' });
}

// numberFormat formats an integer with thousands separators ('1,234').
// Falls back to plain String(n) on a non-finite input. Pulled out so
// the indicator label reads cleanly when the user has 47k history
// events.
function numberFormat(n) {
  if (!Number.isFinite(n)) return String(n);
  try {
    return n.toLocaleString();
  } catch (_) {
    return String(n);
  }
}

// PageIndicator renders the static middle button of the join group.
// It's a `<button disabled>` rather than a span so the DaisyUI join
// styling (rounded ends + matching height) applies cleanly to a
// uniform set of buttons.
function PageIndicator(label) {
  return m('button', {
    type: 'button',
    class: 'btn btn-sm join-item btn-disabled pointer-events-none',
    'aria-current': 'page',
  }, label);
}

// indicatorLabel returns the human-readable middle-button text.
// showRange=true → "Showing X–Y of Z" (the "events / rows visible"
// framing); default → "Page N of M" (the "navigation" framing).
function indicatorLabel({ offset, limit, total, showRange }) {
  if (showRange) {
    const start = total === 0 ? 0 : offset + 1;
    const end = Math.min(offset + limit, total);
    return 'Showing ' + numberFormat(start) + '–' + numberFormat(end) +
      ' of ' + numberFormat(total);
  }
  const totalPages = Math.max(1, Math.ceil(total / limit));
  const currentPage = Math.floor(offset / limit) + 1;
  return 'Page ' + numberFormat(currentPage) + ' of ' + numberFormat(totalPages);
}

const Pagination = {
  view(vnode) {
    const {
      offset = 0,
      limit = 50,
      total = 0,
      setOffset,
      showRange = false,
    } = vnode.attrs;

    // Single page → no need for the chrome. Bailing here keeps the
    // visual rhythm clean on small libraries.
    if (total <= limit && offset === 0) return null;

    const safeLimit = Math.max(1, Number(limit) || 50);
    const prevDisabled = offset <= 0;
    const nextDisabled = offset + safeLimit >= total;

    function gotoPrev() {
      if (prevDisabled) return;
      const next = Math.max(0, offset - safeLimit);
      setOffset(next);
      scrollToTop();
    }

    function gotoNext() {
      if (nextDisabled) return;
      setOffset(offset + safeLimit);
      scrollToTop();
    }

    return m('nav', {
      class: 'flex justify-center mt-4',
      'aria-label': 'Pagination',
    }, m('div', { class: 'join' }, [
      m('button', {
        type: 'button',
        class: 'btn btn-sm join-item' + (prevDisabled ? ' btn-disabled' : ''),
        disabled: prevDisabled,
        onclick: gotoPrev,
        'aria-label': 'Previous page',
      }, '« Prev'),
      PageIndicator(indicatorLabel({
        offset, limit: safeLimit, total, showRange,
      })),
      m('button', {
        type: 'button',
        class: 'btn btn-sm join-item' + (nextDisabled ? ' btn-disabled' : ''),
        disabled: nextDisabled,
        onclick: gotoNext,
        'aria-label': 'Next page',
      }, 'Next »'),
    ]));
  },
};

export default Pagination;
