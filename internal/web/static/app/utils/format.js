// utils/format.js — small display formatters shared across pages.
//
// smartDate is ported verbatim from the legacy library.js so the SPA
// renders dates with the same precision tier the catalog recorded:
//   full date known           → YYYY-MM-DD
//   day unknown, month known  → YYYY-MM
//   month unknown             → YYYY
//   no date at all            → —
// The dash placeholder matches the rest of the UI's "missing value"
// convention.

export function smartDate(full, monthKnown, dayKnown) {
  if (!full) return '—'; // em dash, matches legacy library.js.
  if (!monthKnown) return String(full).substring(0, 4);
  if (!dayKnown) return String(full).substring(0, 7);
  return String(full).substring(0, 10);
}

// relativeTime returns a coarse "N units ago" string for an ISO
// timestamp, or '—' when the input is empty/invalid. Used by
// pages that want a human-friendly age column (history, queue).
// Granularity stops at days — the UI never needs hours-and-minutes
// precision for the activity log.
export function relativeTime(iso) {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '—';
  const sec = Math.round((Date.now() - t) / 1000);
  if (sec < 60) return 'just now';
  const min = Math.round(sec / 60);
  if (min < 60) return min + 'm ago';
  const hr = Math.round(min / 60);
  if (hr < 24) return hr + 'h ago';
  const day = Math.round(hr / 24);
  return day + 'd ago';
}

// humanSize mirrors internal/web/web.go's humanSize (IEC binary, 2dp,
// abbreviated KB/MB/GB/TB). Values under 1 KiB render as raw bytes.
const KiB = 1024;
const MiB = KiB * 1024;
const GiB = MiB * 1024;
const TiB = GiB * 1024;

export function humanSize(b) {
  const n = Number(b) || 0;
  if (n >= TiB) return (n / TiB).toFixed(2) + ' TB';
  if (n >= GiB) return (n / GiB).toFixed(2) + ' GB';
  if (n >= MiB) return (n / MiB).toFixed(2) + ' MB';
  if (n >= KiB) return (n / KiB).toFixed(2) + ' KB';
  return n + ' B';
}

// MONTHS_LONG is the locale-independent month list used by
// formatNFTDate. Hard-coded so the NFT callout renders the same
// "Month D, YYYY" string regardless of the visitor's browser locale.
const MONTHS_LONG = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
];

// formatNFTDate renders an ISO date or RFC3339 timestamp as
// "Month D, YYYY". Returns the raw input on parse failure and an
// empty string for empty input. Mirrors the legacy
// recording.js helper so the NFT-callout copy stays byte-identical.
export function formatNFTDate(s) {
  if (!s) return '';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  return MONTHS_LONG[d.getMonth()] + ' ' + d.getDate() + ', ' + d.getFullYear();
}

// errorMessage extracts a human-readable string from a fetch failure
// produced by api.js. The api wrapper attaches the response body to
// .body — usually a JSON {message: "..."} from echo's NewHTTPError,
// occasionally a plain string. Falls back to .message and finally
// to String(err) so callers always get something to show.
export function errorMessage(err) {
  if (!err) return 'unknown error';
  if (err.body) {
    try {
      const parsed = JSON.parse(err.body);
      if (parsed && parsed.message) return String(parsed.message);
      if (parsed && parsed.error) return String(parsed.error);
    } catch (_) { /* not JSON; fall through. */ }
    if (typeof err.body === 'string' && err.body.length < 240) return err.body;
  }
  return err.message || String(err);
}
