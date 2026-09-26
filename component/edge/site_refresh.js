/* Installed by the hosting layer on every HTML page, independent of framework. */
(function () {
  "use strict";
  var script = document.currentScript;
  var version = script && script.getAttribute("data-memql-version");
  if (!version || window.__memqlSiteRefresh) return;
  window.__memqlSiteRefresh = true;
  var key = "memql:site-refresh";
  var marker = "__memql_reload";
  var interval = 30000;
  var cooldown = 60000;
  var dirty = false;
  var busy = false;
  var navigating = false;
  var candidate = "";
  var confirmation;
  var lastCheck = 0;
  var lastReload = 0;
  var pathname = location.pathname;
  var current = new URL(location.href);
  function reloadTime(value) {
    var time = Number(value);
    return Number.isFinite(time) && time > 0 && time <= Date.now() ? time : 0;
  }
  var stamp = reloadTime(current.searchParams.get(marker));
  try {
    lastReload = Math.max(stamp, reloadTime(sessionStorage.getItem(key)));
    if (stamp) {
      sessionStorage.setItem(key, String(lastReload));
      current.searchParams.delete(marker);
      history.replaceState(history.state, "", current.href);
    }
  } catch (_) {
    // The URL marker retains the loop guard when browser storage is disabled.
    lastReload = stamp;
  }

  function reload() {
    if (dirty || navigating || document.visibilityState === "hidden") return;
    var now = Date.now();
    if (now - lastReload < cooldown) return;
    lastReload = now;
    try { sessionStorage.setItem(key, String(now)); } catch (_) {}
    var next = new URL(location.href);
    // A new document URL also bypasses legacy HTTP entries written before the
    // revalidation policy existed. Keep the route, other query values and hash.
    next.searchParams.set(marker, String(now));
    navigating = true;
    location.replace(next.href);
  }

  async function check() {
    if (busy || navigating || document.visibilityState === "hidden") return;
    var now = Date.now();
    if (now - lastCheck < 2000) return;
    lastCheck = now;
    busy = true;
    var controller = new AbortController();
    var timeout = setTimeout(function () { controller.abort(); }, 5000);
    try {
      var response = await fetch("/runtime-config.json", {
        method: "HEAD", cache: "no-store", credentials: "same-origin", signal: controller.signal
      });
      var latest = response.ok && response.headers.get("X-MemQL-Deployment");
      if (!latest || !/^[a-f0-9]{32}$/.test(latest)) return;
      if (latest === version) {
        candidate = "";
        return;
      }
      // Two agreeing observations avoid reacting to a single old replica
      // during a rolling upgrade. The per-tab cooldown bounds any remaining
      // oscillation, including rollbacks and temporary replica disagreement.
      if (candidate === latest) {
        reload();
      } else {
        candidate = latest;
        clearTimeout(confirmation);
        confirmation = setTimeout(check, 2500);
      }
    } catch (_) {
      // An offline browser or a failed check retains the working page. The
      // online/focus events and periodic check retry without a reload loop.
    } finally {
      clearTimeout(timeout);
      busy = false;
    }
  }

  function routeChanged() {
    if (location.pathname !== pathname) {
      pathname = location.pathname;
      dirty = false;
    }
    void check();
  }
  // A generic host cannot reconstruct an application's unsaved form state.
  // Defer background reloads after edits until the user leaves that route.
  document.addEventListener("input", function (event) {
    var target = event.target;
    if (target && target.closest && target.closest("input, textarea, select, [contenteditable]")) dirty = true;
  }, true);
  document.addEventListener("change", function (event) {
    var target = event.target;
    if (target && target.closest && target.closest("input, textarea, select, [contenteditable]")) dirty = true;
  }, true);
  document.addEventListener("submit", function () { dirty = true; }, true);
  document.addEventListener("visibilitychange", check);
  window.addEventListener("pageshow", check);
  window.addEventListener("focus", check);
  window.addEventListener("online", check);
  window.addEventListener("popstate", routeChanged);
  document.addEventListener("click", function (event) {
    if (event.target && event.target.closest && event.target.closest("a[href]")) void check();
  }, true);
  ["pushState", "replaceState"].forEach(function (method) {
    var original = history[method];
    history[method] = function () {
      var result = original.apply(this, arguments);
      routeChanged();
      return result;
    };
  });
  setInterval(check, interval);
  void check();
})();
