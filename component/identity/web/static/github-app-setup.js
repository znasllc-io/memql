/*
 * github-app-setup.js
 * ===================
 *
 * Submits the one form on the GitHub App setup page as it loads (design
 * record 2026-09-20-github-app-setup, D4).
 *
 * WHAT IT IS FOR. Registering a cluster's GitHub App starts with a browser
 * form POST to github.com, and this page exists only to make it -- MemQL OS
 * cannot, under the policy the edge serves every hosted site with. Somebody
 * who pressed "Set up GitHub" a moment ago has already said yes; a second
 * button on a page they did not ask for is a step, not a confirmation. GitHub
 * asks the real question on its own page.
 *
 * PROGRESSIVE ENHANCEMENT. With this script blocked the page is a sentence
 * and a button that does the same thing.
 *
 * ONCE. A person who comes BACK to this page with the browser's back button
 * is shown the button rather than thrown forward again -- otherwise Back from
 * GitHub is a loop. `pageshow` with `persisted` is the back-forward cache
 * saying the page was restored, not loaded.
 *
 * Obeys the strict CSP (csp.go): no inline handlers, no eval, no remote
 * sources.
 */

(function () {
  "use strict";

  var restored = false;
  window.addEventListener("pageshow", function (event) {
    if (event.persisted) restored = true;
  });

  document.addEventListener("DOMContentLoaded", function () {
    var form = document.querySelector("form[data-github-app-setup]");
    if (!form || restored) return;
    var entries = window.performance && performance.getEntriesByType
      ? performance.getEntriesByType("navigation")
      : [];
    if (entries.length > 0 && entries[0].type === "back_forward") return;
    form.submit();
  });
})();
