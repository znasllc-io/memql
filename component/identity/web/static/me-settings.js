/*
 * me-settings.js
 * ==============
 *
 * Loader for /me/settings. Fills the profile + legal-acceptance
 * panels by calling /me/api/profile and /me/api/legal-acceptance.
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

  function fillLegal(items) {
    var zone = document.querySelector('[data-content="legal"]');
    if (!zone) return;
    if (!items || !items.length) {
      zone.innerHTML = '<p class="text-muted">No acceptance records on file yet.</p>';
      return;
    }
    var html = '<table class="data"><tr><th>Document</th><th>Version</th><th>Accepted at</th></tr>';
    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      html += "<tr><td>" + escapeHTML(it.documentType || "") +
              "</td><td>" + escapeHTML(it.version || "") +
              "</td><td>" + escapeHTML(it.acceptedAt || "") + "</td></tr>";
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

    fetch("/me/api/legal-acceptance", { headers: headers })
      .then(function (r) { return r.json(); })
      .then(function (data) { fillLegal(data && data.items); })
      .catch(function () {
        var zone = document.querySelector('[data-content="legal"]');
        if (zone) zone.innerHTML = '<p class="text-danger">Could not load acceptance history.</p>';
      });
  });
})();
