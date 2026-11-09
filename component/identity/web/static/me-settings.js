/*
 * me-settings.js
 * ==============
 *
 * Loader for /me/settings. Fills the profile panel by calling
 * /me/api/profile. The legal-acceptance card is server-rendered from the
 * caller's own row (me_legal_acceptance.go); this script must not touch it,
 * because the fetch it used to make went to an endpoint nothing mounted and
 * replaced the card with its error.
 */

(function () {
  if (!window.IdentityMe || !window.IdentityMe.register) return;

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;")
      .replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function fillProfile(data) {
    var zone = document.querySelector('[data-content="profile"]');
    if (!zone) return;
    var rows = [
      ["Display name", data.displayName || "(not set)"],
      ["Primary email", data.primaryEmail || "(unknown)"],
      ["Cluster role", data.role || "(none)"],
      ["Internal user", data.internal ? "Yes" : "No"],
    ];
    var html = '<table class="data">';
    for (var i = 0; i < rows.length; i++) {
      html += "<tr><th>" + escapeHTML(rows[i][0]) + "</th><td>" + escapeHTML(rows[i][1]) + "</td></tr>";
    }
    html += "</table>";
    zone.innerHTML = html;
  }

  window.IdentityMe.register(function (token) {
    var headers = { "Authorization": "Bearer " + token, "Accept": "application/json" };
    fetch("/me/api/profile", { headers: headers })
      .then(function (r) { return r.json(); })
      .then(fillProfile)
      .catch(function () {
        var zone = document.querySelector('[data-content="profile"]');
        if (zone) zone.innerHTML = '<p class="text-danger">Could not load profile.</p>';
      });
  });
})();
