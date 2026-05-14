/*
 * app.js
 * ======
 *
 * Progressive-enhancement JavaScript for the identity web app.
 *
 * The HTML pages WORK WITHOUT THIS SCRIPT — every form posts to a
 * server-side handler, every link is a real anchor. JavaScript is
 * additive: inline validation, button-disable on submit, and the
 * /me/* dashboard's bearer-token bootstrap.
 *
 * The runtime obeys the strict CSP enforced by csp.go: no inline
 * event handlers, no eval, no remote sources. All registered
 * behaviors target elements via data-* attributes and wire up
 * once on DOMContentLoaded.
 *
 * Stimulus is loaded as a separate <script> (see layout.html) and
 * is the canonical pattern for any non-trivial page interaction.
 * Define a controller in static/<name>_controller.js, mark the HTML
 * with data-controller="<name>", and let Stimulus wire it. This
 * file's job is the global progressive-enhancement glue (form
 * submit-once, email hint) plus the Stimulus application bootstrap.
 */

(function () {
  "use strict";

  // -------------------------------------------------------------
  // Form helpers — disable submit + show spinner on POST forms
  // marked data-submit-once.
  // -------------------------------------------------------------
  function wireSubmitOnce() {
    var forms = document.querySelectorAll("form[data-submit-once]");
    for (var i = 0; i < forms.length; i++) {
      (function (form) {
        form.addEventListener("submit", function () {
          var btn = form.querySelector('button[type="submit"], input[type="submit"]');
          if (btn) {
            btn.disabled = true;
            var label = btn.getAttribute("data-pending-label");
            if (label) {
              btn.textContent = label;
            }
          }
        });
      })(forms[i]);
    }
  }

  // -------------------------------------------------------------
  // Email-field inline validation (HTML5 + a small custom hint).
  // -------------------------------------------------------------
  function wireEmailHint() {
    var inputs = document.querySelectorAll('input[type="email"][data-hint-target]');
    for (var i = 0; i < inputs.length; i++) {
      (function (input) {
        var target = document.querySelector(input.getAttribute("data-hint-target"));
        if (!target) return;
        input.addEventListener("input", function () {
          var v = (input.value || "").trim();
          if (!v) {
            target.textContent = "";
            return;
          }
          if (v.indexOf("@") < 0 || v.indexOf(".", v.indexOf("@")) < 0) {
            target.textContent = "Looks incomplete — make sure your address has an @ and a domain.";
          } else {
            target.textContent = "";
          }
        });
      })(inputs[i]);
    }
  }

  // -------------------------------------------------------------
  // /me/* dashboard bootstrap.
  //
  // The /me pages are static HTML shells. The actual user data is
  // fetched through the standard SPA flow:
  //
  //   1. POST /auth/refresh (with credentials) to exchange the
  //      identity-domain cookie for an access token.
  //   2. If 401: hard-redirect to /login?return_to=<current>.
  //   3. If 200: stash the access token in memory and call
  //      registered loaders that fill data-content zones.
  //
  // Page-specific behavior registers via window.IdentityMe.register
  // before this bootstrap runs (script tags in the page templates
  // load app.js LAST, after their own small setup blocks).
  // -------------------------------------------------------------
  var loaders = [];
  window.IdentityMe = {
    register: function (fn) { loaders.push(fn); },
    accessToken: null,
  };

  function meBootstrap() {
    if (!document.body.hasAttribute("data-me")) return;

    fetch("/auth/refresh", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    }).then(function (resp) {
      if (resp.status === 401 || resp.status === 403) {
        var here = window.location.pathname + window.location.search;
        window.location.replace("/login?return_to=" + encodeURIComponent(here));
        return null;
      }
      return resp.json();
    }).then(function (data) {
      if (!data || !data.access_token) return;
      window.IdentityMe.accessToken = data.access_token;
      runLoaders();
    }).catch(function () {
      var zones = document.querySelectorAll("[data-content]");
      for (var i = 0; i < zones.length; i++) {
        zones[i].textContent = "Could not load this page. Please refresh.";
      }
    });
  }

  function runLoaders() {
    for (var i = 0; i < loaders.length; i++) {
      try {
        loaders[i](window.IdentityMe.accessToken);
      } catch (e) {
        // Loader errors are non-fatal — surface in console for ops.
        if (window.console) { console.error("identity-me loader failed", e); }
      }
    }
  }

  // -------------------------------------------------------------
  // Stimulus application bootstrap.
  //
  // Pages that need interactive behaviour declare it via
  // data-controller="<name>" on a container, plus data-action and
  // data-<name>-target on its descendants. The controller class
  // lives in static/<name>_controller.js and registers itself by
  // calling window.IdentityStimulus.register("<name>", Controller)
  // before app.js boots (script order in layout: controller files
  // load FIRST via ExtraScripts, then app.js calls Application.start).
  //
  // We expose a tiny façade rather than the raw window.Stimulus
  // object so controller files don't need to know whether the
  // application has booted yet — they queue registrations and we
  // apply them as soon as Application.start() runs.
  // -------------------------------------------------------------
  var pendingControllers = [];
  window.IdentityStimulus = {
    register: function (name, ctrl) {
      pendingControllers.push({ name: name, ctrl: ctrl });
      flushControllers();
    },
    application: null,
  };

  function startStimulus() {
    if (window.IdentityStimulus.application) return;
    if (!window.Stimulus) {
      // app.js is the first defer script in the layout, so when boot()
      // fires from DOMContentLoaded the OTHER defer scripts (htmx,
      // stimulus, page extras) MAY still be in flight. Retry on
      // window.load, by which point every script tag has resolved.
      // `once:true` keeps the handler from re-binding across boots.
      window.addEventListener("load", startStimulus, { once: true });
      if (window.console) {
        console.info("[identity] Stimulus not loaded yet; deferring start to window.load");
      }
      return;
    }
    window.IdentityStimulus.application = window.Stimulus.Application.start();
    if (window.console) {
      console.info("[identity] Stimulus application started; flushing", pendingControllers.length, "queued controllers");
    }
    flushControllers();
  }

  function flushControllers() {
    var app = window.IdentityStimulus.application;
    if (!app) return;
    while (pendingControllers.length) {
      var p = pendingControllers.shift();
      try {
        app.register(p.name, p.ctrl);
        if (window.console) {
          console.info("[identity] registered Stimulus controller:", p.name);
        }
      } catch (e) {
        if (window.console) { console.error("[identity] stimulus register failed", p.name, e); }
      }
    }
  }

  // -------------------------------------------------------------
  // data-confirm — replacement for inline onclick=confirm(...).
  // Forms that need a "are you sure?" gate carry data-confirm with
  // the prompt text; a submit handler shows confirm() and bails out
  // when the user cancels. Strict CSP disallows inline handlers, so
  // this lives here instead.
  // -------------------------------------------------------------
  function wireConfirm() {
    var els = document.querySelectorAll("[data-confirm]");
    for (var i = 0; i < els.length; i++) {
      (function (el) {
        var prompt = el.getAttribute("data-confirm");
        if (!prompt) return;
        el.addEventListener("submit", function (ev) {
          if (!window.confirm(prompt)) {
            ev.preventDefault();
            ev.stopPropagation();
          }
        });
      })(els[i]);
    }
  }

  // -------------------------------------------------------------
  // Boot
  // -------------------------------------------------------------
  function boot() {
    wireSubmitOnce();
    wireConfirm();
    wireEmailHint();
    meBootstrap();
    startStimulus();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
