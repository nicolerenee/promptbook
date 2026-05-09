// recording.js — placeholder. The recording detail page is rebuilt in a
// later wave (Wave 9 ships only the library page as the reference).
//
// IMPORTANT — when this page is implemented, the NFT callout copy must
// be the corrected version (the design's copy is wrong):
//   - With date:    "Not For Trade — until YYYY-MM-DD."
//                   "Do not share or trade this recording until the date passes."
//   - Forever:      "Not For Trade — permanent."
//                   "Do not share or trade this recording."
//   "NFT" stands for "Not For Trade" — NOT the design's "no-further-trade".
//   Drop the design's "Promptbook will not include it in batch shares or
//   public catalogs" sentence — we don't have that functionality.

(function () {
  'use strict';
  var root = document.getElementById('page-root');
  if (!root || root.getAttribute('data-page') !== 'recording') return;
  console.log('TODO: implement recording detail page');
  root.innerHTML = '<div class="pb-empty">' +
    'This page is being rebuilt in the new design. Coming soon.' +
    '</div>';
})();
