/*
 * me-dashboard.js
 * ===============
 *
 * Loader for /me/ overview. Calls /me/api/profile with the bearer
 * token IdentityMe.accessToken and renders into [data-content="overview"].
 */

(function () {
  if (!window.IdentityMe || !window.IdentityMe.register) return;

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;")
      .replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  window.IdentityMe.register(function (token) {
    fetch("/me/api/profile", {
      headers: { "Authorization": "Bearer " + token, "Accept": "application/json" },
    }).then(function (r) {
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    }).then(function (data) {
      var zone = document.querySelector('[data-content="overview"]');
      if (!zone) return;
      var rows = [
        ["Name", data.displayName || "(not set)"],
        ["Primary email", data.primaryEmail || "(unknown)"],
        ["Cluster role", data.role || "(none)"],
        ["Member since", data.createdAt || ""],
      ];
      var html = '<table class="data">';
      for (var i = 0; i < rows.length; i++) {
        html += "<tr><th>" + escapeHTML(rows[i][0]) + "</th><td>" + escapeHTML(rows[i][1]) + "</td></tr>";
      }
      html += "</table>";
      zone.innerHTML = html;
    }).catch(function () {
      var zone = document.querySelector('[data-content="overview"]');
      if (zone) zone.innerHTML = '<p class="text-danger">Could not load profile.</p>';
    });
  });
})();
