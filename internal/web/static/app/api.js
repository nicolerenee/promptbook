// api.js — thin m.request wrapper around the JSON API at /api/v1/*.
//
// The legacy api.js global (window.PB.api) is the same shape — fetch
// helper that rejects on non-2xx with a normalized Error. This module
// uses Mithril's m.request so an in-flight request triggers an auto
// redraw on resolution. The legacy .get/.post names are preserved for
// muscle memory across the codebase.

import m from 'https://esm.sh/mithril@2.2.2';

const BASE = '/api/v1';

// normalizeError takes whatever m.request rejected with and returns an
// Error with .status (if known) + .body (response text when available)
// so handlers can display a useful message without each one duplicating
// the unwrap logic.
function normalizeError(reason, url) {
  const err = new Error(
    `HTTP ${reason && reason.code ? reason.code : 'error'} on ${url}`,
  );
  if (reason && reason.code != null) err.status = reason.code;
  if (reason && reason.response != null) err.body = reason.response;
  if (reason && reason.message) err.message = reason.message;
  return err;
}

export function get(path) {
  const url = BASE + path;
  return m.request({
    url,
    method: 'GET',
    withCredentials: false,
    headers: { Accept: 'application/json' },
  }).catch((reason) => { throw normalizeError(reason, url); });
}

export function post(path, body) {
  const url = BASE + path;
  return m.request({
    url,
    method: 'POST',
    body,
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
  }).catch((reason) => { throw normalizeError(reason, url); });
}

export default { get, post };
