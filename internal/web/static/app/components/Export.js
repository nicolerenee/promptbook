// Export.js — the /export page. Renders a pipe-delimited code block
// listing every recording the user actually has on disk, sorted by
// show name. Columns: Show | Tour | Date (with variant) | Master |
// Format. Use case: copy-paste the table into Discord / a forum / a
// spreadsheet so the user can share their collection without
// hand-typing rows.
//
// Filter: fileCount > 0. "Recordings I have" means at least one
// recording_versions row exists on disk — covers synced + format-
// mismatch + the rare "I have it but only as a want" edge case.
// Wanted-but-no-file recordings stay out (they're aspirational, not
// owned — see the project memory note on Encora trading).

import m from 'https://esm.sh/mithril@2.2.2';
import graphql from '../graphql.js';
import { smartDateWithVariant, errorMessage } from '../utils/format.js';
import state from '../state.js';

// EXPORT_QUERY mirrors the recordings-list resolver but only selects
// the fields the export needs. Single fetch — the same 100k limit
// the catalog list uses (Encora collections cap well under that).
const EXPORT_QUERY = `
  query CollectionExport($limit: Int) {
    recordingsList(limit: $limit) {
      items {
        id
        show
        tour
        dateFull
        dateMonthKnown
        dateDayKnown
        dateVariant
        master
        fileCount
        localReleaseFormat
      }
    }
  }
`;

const EXPORT_LIMIT = 100000;

// formatRow turns one recording into the pipe-delimited row the
// code block renders. Empty values render as "—" so the column
// shape stays consistent.
function formatRow(node) {
  const show = node.show || '—';
  const tour = node.tour || '—';
  const date = smartDateWithVariant(
    node.dateFull,
    !!node.dateMonthKnown,
    !!node.dateDayKnown,
    node.dateVariant,
  );
  const master = node.master || '—';
  const format = node.localReleaseFormat || '—';
  return [show, tour, date || '—', master, format].join(' | ');
}

// loadExport fetches the recordings list and stores the formatted
// text on state.export. Wrapped so onbeforeremove + retry can re-
// trigger without duplicating the query call.
function loadExport() {
  const s = state.export;
  s.loading = true;
  s.error = null;
  return graphql.query(EXPORT_QUERY, { limit: EXPORT_LIMIT })
    .then((data) => {
      const items = (data && data.recordingsList && data.recordingsList.items) || [];
      // Filter to "I actually have" — fileCount > 0 — then sort by
      // show name (case-insensitive) with date as the tiebreaker so
      // multiple captures of the same show stay grouped.
      const owned = items.filter((n) => n && (n.fileCount || 0) > 0);
      owned.sort((a, b) => {
        const showA = (a.show || '').toLowerCase();
        const showB = (b.show || '').toLowerCase();
        if (showA !== showB) return showA < showB ? -1 : 1;
        const dateA = a.dateFull || '';
        const dateB = b.dateFull || '';
        return dateA < dateB ? -1 : dateA > dateB ? 1 : 0;
      });
      s.text = owned.map(formatRow).join('\n');
      s.count = owned.length;
      s.loading = false;
    })
    .catch((err) => {
      s.loading = false;
      s.error = errorMessage(err);
    });
}

// copyToClipboard copies the rendered text to the system clipboard.
// Uses navigator.clipboard when available (modern browsers); falls
// back to a hidden textarea + execCommand for older Edge / Safari.
// Flips state.export.copied true for 2s so the button can show a
// "Copied" affordance.
function copyToClipboard() {
  const s = state.export;
  const text = s.text || '';
  if (!text) return;
  const finish = () => {
    s.copied = true;
    m.redraw();
    setTimeout(() => {
      s.copied = false;
      m.redraw();
    }, 2000);
  };
  if (navigator && navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(finish).catch(() => {
      // Fall through to the legacy path if the modern API rejects.
      legacyCopy(text);
      finish();
    });
    return;
  }
  legacyCopy(text);
  finish();
}

function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.left = '-9999px';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (_) { /* best-effort */ }
  document.body.removeChild(ta);
}

const Export = {
  oninit() {
    if (!state.export) {
      state.export = { loading: true, error: null, text: '', count: 0, copied: false };
    }
    loadExport();
  },

  view() {
    const s = state.export || { loading: true, error: null, text: '', count: 0, copied: false };
    const headerSub = s.loading
      ? 'Loading…'
      : s.error
        ? 'Error loading recordings'
        : String(s.count) + ' recording' + (s.count === 1 ? '' : 's') + ' on disk';
    return m('div', { class: 'space-y-4' }, [
      m('header', { class: 'flex items-start justify-between gap-4 flex-wrap' }, [
        m('div', [
          m('h1', { class: 'text-3xl font-semibold' }, 'Export'),
          m('p', { class: 'text-sm opacity-70 mt-1' }, headerSub),
          m('p', { class: 'text-xs opacity-60 mt-1' },
            'Pipe-delimited list of every recording with files on disk, ' +
            'sorted by show name. Copy + paste into Discord / a forum / a ' +
            'spreadsheet to share what you have.'),
        ]),
        m('div', { class: 'flex items-center gap-2' }, [
          m('button', {
            type:     'button',
            class:    'btn btn-primary btn-sm',
            disabled: s.loading || !!s.error || !s.text,
            onclick:  copyToClipboard,
          }, s.copied ? 'Copied' : 'Copy to clipboard'),
        ]),
      ]),
      s.error
        ? m('div', { role: 'alert', class: 'alert alert-error' },
            m('span', 'Failed to load: ' + s.error))
        : null,
      s.loading
        ? m('div', { class: 'p-8 opacity-60' }, 'Loading…')
        : m('pre', {
            class: 'p-4 rounded bg-base-200 text-xs font-mono ' +
                   'overflow-x-auto whitespace-pre',
          }, s.text || '(no recordings on disk yet)'),
    ]);
  },
};

export default Export;
