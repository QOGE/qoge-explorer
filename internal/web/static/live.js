/*
 * QOGE Explorer — confirmed-chain live UI refresh (Phase 2E.2).
 *
 * Architectural rule: this script is NOT a second explorer engine. It only
 * polls GET /api/v1/status for indexed_height/indexed_block_hash, decides
 * whether the current page's baseline tip is stale, and either triggers a
 * normal full-page reload (obtaining a fresh, coherent server-rendered
 * snapshot from query.Store) or shows a passive notification banner asking
 * the user to refresh manually. It never fetches additional API objects,
 * never reconstructs blockchain state in the DOM, and never touches
 * addresses/scripts/witness bytes/transaction data/money values.
 *
 * Auto-reload is only attempted on pages that mark themselves eligible via
 * a `data-live-refresh="home"` or `data-live-refresh="blocks"` element
 * (see templates/home.tmpl and templates/blocks.tmpl) — every other page
 * (block/tx/address detail, paginated /blocks, ?include_witness=true) only
 * shows the banner and lets the user decide when to reload, so a raw
 * 17,088-byte P2QPK witness view, scroll position, or historical/orphan
 * inspection is never disrupted by a background refresh.
 */
(function () {
  "use strict";

  var STATUS_URL = "/api/v1/status";
  var POLL_INTERVAL_MS = 10000;

  var requestInFlight = false;

  var refreshEl = document.querySelector("[data-live-refresh]");
  var refreshMode = refreshEl ? refreshEl.getAttribute("data-live-refresh") : null;

  var banner = document.getElementById("live-chain-banner");
  var bannerMessage = banner ? banner.querySelector("[data-live-banner-message]") : null;
  var bannerRefresh = banner ? banner.querySelector("[data-live-banner-refresh]") : null;

  function parseIntAttr(el, name) {
    if (!el) {
      return null;
    }
    var raw = el.getAttribute(name);
    if (raw === null || raw === "") {
      return null;
    }
    var n = parseInt(raw, 10);
    return isNaN(n) ? null : n;
  }

  // baselineHeight/baselineHash describe the tip this PAGE was rendered
  // from — used only to decide reload-vs-notify for eligible pages, never
  // rendered or reconstructed into new DOM content.
  var baselineHeight = null;
  var baselineHash = null;

  if (refreshMode === "home") {
    baselineHeight = parseIntAttr(refreshEl, "data-indexed-height");
    baselineHash = refreshEl.getAttribute("data-indexed-hash") || null;
  } else if (refreshMode === "blocks") {
    var firstRow = refreshEl.querySelector("[data-block-height]");
    baselineHeight = parseIntAttr(firstRow, "data-block-height");
    baselineHash = firstRow ? (firstRow.getAttribute("data-block-hash") || null) : null;
  }

  var lastKnownHash = baselineHash;

  function reloadPage() {
    window.location.reload();
  }

  function showBanner() {
    if (!banner) {
      return;
    }
    if (bannerMessage) {
      bannerMessage.textContent = "The indexed canonical tip changed.";
    }
    banner.hidden = false;
  }

  if (bannerRefresh) {
    bannerRefresh.addEventListener("click", reloadPage);
  }

  // handleStatus decides reload-vs-notify. The signal is "indexed_block_hash
  // changed" — never assumed to mean "a new block arrived": a reorg can
  // change the canonical hash at the same height, and rollback/replacement
  // can transiently move height backward before a replacement block lands.
  // Reload is only safe once the new height is at or above this page's own
  // baseline height; otherwise we're most likely observing the rollback
  // half of a reorg in flight, so we notify and let the next poll observe
  // the stabilized tip.
  function handleStatus(status) {
    if (!status || typeof status.indexed_height !== "number") {
      return;
    }
    var height = status.indexed_height;
    var hash = typeof status.indexed_block_hash === "string" ? status.indexed_block_hash : null;

    if (hash === lastKnownHash) {
      return;
    }
    lastKnownHash = hash;

    if ((refreshMode === "home" || refreshMode === "blocks") &&
        (baselineHeight === null || height >= baselineHeight)) {
      reloadPage();
      return;
    }

    showBanner();
  }

  function poll() {
    if (requestInFlight || document.hidden) {
      return;
    }
    requestInFlight = true;
    fetch(STATUS_URL, {
      cache: "no-store",
      headers: { "Accept": "application/json" }
    }).then(function (resp) {
      if (!resp.ok) {
        throw new Error("status request failed");
      }
      return resp.json();
    }).then(function (data) {
      handleStatus(data);
    }).catch(function () {
      // A temporary failed status request retains the last known
      // checkpoint and simply retries on the next poll — never reloads,
      // never shows a false chain warning, never breaks navigation.
    }).then(function () {
      requestInFlight = false;
    });
  }

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      poll();
    }
  });

  poll();
  window.setInterval(poll, POLL_INTERVAL_MS);
})();
