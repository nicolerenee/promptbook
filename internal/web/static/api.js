// api.js — tiny fetch helper for the JSON API at /api/v1/*. Pages call
// window.PB.api.get('/recordings') and get back a Promise<JSON>. Non-2xx
// responses reject so callers can distinguish loaded-but-empty from
// failures.

(function () {
  'use strict';

  var BASE = '/api/v1';

  function asJSON(res) {
    if (!res.ok) {
      var err = new Error('HTTP ' + res.status + ' on ' + res.url);
      err.status = res.status;
      return res.text().then(function (body) {
        err.body = body;
        throw err;
      }, function () { throw err; });
    }
    return res.json();
  }

  function get(path) {
    return fetch(BASE + path, {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin',
    }).then(asJSON);
  }

  function post(path, body) {
    return fetch(BASE + path, {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json',
      },
      credentials: 'same-origin',
      body: JSON.stringify(body),
    }).then(asJSON);
  }

  window.PB = window.PB || {};
  window.PB.api = { get: get, post: post };
})();
