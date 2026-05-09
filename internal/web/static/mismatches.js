// mismatches.js — placeholder. Implemented in a later wave.
//
// When implemented, the apply form will POST to /apply (HTML) or
// /api/v1/apply (JSON) — both routes are already wired in the server.
// The current minimal apply form is gone; the JS rebuild will need to
// reproduce the checkbox + selection UI.
(function () {
  'use strict';
  var root = document.getElementById('page-root');
  if (!root || root.getAttribute('data-page') !== 'mismatches') return;
  console.log('TODO: implement mismatches page');
  root.innerHTML = '<div class="pb-empty">' +
    'This page is being rebuilt in the new design. Coming soon.' +
    '</div>';
})();
