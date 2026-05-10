// graphql.js — thin m.request wrapper around the /graphql endpoint.
//
// Mirrors api.js's shape so the muscle memory carries: rejects on
// non-2xx with a normalized Error, uses Mithril's m.request so an
// in-flight query triggers a redraw on resolution. Distinct from
// api.js because the endpoint is /graphql (not under /api/v1) and the
// JSON body envelope is { data, errors } instead of the REST shape.
//
// Rules:
// - POST { query, variables } against /graphql.
// - Resolve to `data` when the response has no `errors` array.
// - Reject with an Error whose .message concatenates the GraphQL
//   error messages when `errors` is present, so callers see "field
//   X must not be null" instead of just "HTTP 200".
//
// No subscriptions, no APQ, no codegen — the SPA stays vanilla JS and
// each page inlines its own query string.

import m from 'https://esm.sh/mithril@2.2.2';

const ENDPOINT = '/graphql';

// normalizeTransportError takes whatever m.request rejected with on a
// transport-level failure (network down, 500, JSON parse error, etc.)
// and returns an Error matching api.js's shape.
function normalizeTransportError(reason) {
  const err = new Error(
    'GraphQL transport error' +
      (reason && reason.code != null ? ' (HTTP ' + reason.code + ')' : ''),
  );
  if (reason && reason.code != null) err.status = reason.code;
  if (reason && reason.response != null) err.body = reason.response;
  if (reason && reason.message) err.message = reason.message;
  return err;
}

// graphqlErrorMessage joins a GraphQL `errors` array into a single
// Error suitable for rendering in an alert. The array always has at
// least one entry when the server returns an `errors` key.
function graphqlErrorMessage(errors) {
  const msgs = errors
    .map((e) => (e && e.message ? String(e.message) : 'GraphQL error'))
    .filter(Boolean);
  return msgs.join('; ') || 'GraphQL error';
}

// query runs a GraphQL query/mutation and resolves to the `data`
// section of the response. Throws a transport error on non-2xx, or a
// GraphQL error when the response carries an `errors` array (even with
// HTTP 200 — gqlgen returns 200 + errors for resolver / validation
// failures by default).
export function query(queryString, variables) {
  return m.request({
    url: ENDPOINT,
    method: 'POST',
    body: { query: queryString, variables: variables || {} },
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
  }).then((body) => {
    if (body && Array.isArray(body.errors) && body.errors.length > 0) {
      const err = new Error(graphqlErrorMessage(body.errors));
      err.graphqlErrors = body.errors;
      throw err;
    }
    return (body && body.data) || {};
  }).catch((reason) => {
    if (reason instanceof Error) throw reason;
    throw normalizeTransportError(reason);
  });
}

export default { query };
