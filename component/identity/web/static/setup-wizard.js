/*
 * setup-wizard.js
 * ===============
 *
 * Plain vanilla JS for the first-run wizard. Two jobs:
 *
 *   1. Disable the Finish-setup submit button until the owner email
 *      AND a registration mode are both filled in.
 *   2. Toggle the conditional sections (allowed registration domains
 *      for domain_restricted mode, admin notify emails for waitlist
 *      mode) based on the selected mode.
 *
 * This file deliberately does NOT use Stimulus. The Stimulus boot
 * chain has had two debugging rounds with the wizard and the cost
 * of getting it wrong (silent failures, button perma-stuck) is
 * higher than the value of using a framework on a 60-line form.
 * The Stimulus runtime stays loaded globally (see layout.html), so
 * future controllers can use it as soon as we have a real reason to.
 *
 * Wiring (in setup_wizard.html):
 *   - The form's container has no data-controller; this file just
 *     queries the DOM directly via input names + a known submit
 *     button selector.
 *   - The conditional rows carry data-when="<mode>" so we can
 *     compute visibility from the selected radio.
 *
 * Loaded only on /setup. The rest of the site doesn't pull this in.
 */

(function () {
  function init() {
    var form = document.querySelector('form[action="/setup"]');
    if (!form) return;
    var emailInput = form.querySelector('#owner_email');
    var submitBtn = form.querySelector('button[type="submit"]');
    var radios = form.querySelectorAll('input[name="registration_mode"]');
    var conditionalRows = form.querySelectorAll('[data-when]');

    function selectedMode() {
      for (var i = 0; i < radios.length; i++) {
        if (radios[i].checked) return radios[i].value;
      }
      return "";
    }

    function emailLooksValid() {
      if (!emailInput) return false;
      var v = (emailInput.value || "").trim();
      if (v.length === 0) return false;
      // Defer to the browser's own HTML5 email validation. The
      // input is `type="email" required`, so checkValidity() returns
      // true only when the value parses as a syntactically-valid
      // email per the WHATWG spec — that's what gets enforced
      // server-side too, so the button enables exactly when the
      // form would actually submit.
      if (typeof emailInput.checkValidity === "function") {
        return emailInput.checkValidity();
      }
      // Fallback regex for ancient browsers without checkValidity.
      // Mirrors the WHATWG-permitted shape (local@domain.tld) but
      // is intentionally simple — server-side does the real work.
      return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v);
    }

    function update() {
      var mode = selectedMode();
      if (submitBtn) {
        submitBtn.disabled = !(mode && emailLooksValid());
      }
      for (var i = 0; i < conditionalRows.length; i++) {
        var row = conditionalRows[i];
        var when = row.getAttribute("data-when") || "";
        row.style.display = (when === mode) ? "" : "none";
      }
    }

    if (emailInput) {
      emailInput.addEventListener("input", update);
      emailInput.addEventListener("change", update);
    }
    for (var i = 0; i < radios.length; i++) {
      radios[i].addEventListener("change", update);
    }
    update();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
