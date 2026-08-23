/*
 * check-email.js
 * ==============
 *
 * The requesting tab's half of the device-bound magic-link flow
 * (memql#4302).
 *
 * WHAT IT IS FOR. A magic link now only COMPLETES in the browser that asked
 * for it. A click anywhere else -- the phone in your pocket, or a colleague
 * reading the same shared mailbox -- only APPROVES the request. This script
 * is what notices the approval and finishes the sign-in here, so that "click
 * on the phone, the laptop signs in" works, and so that a colleague's click
 * signs YOU in rather than them.
 *
 * PROGRESSIVE ENHANCEMENT. With this script blocked the page is exactly what
 * it always was: "check your email". The link still works; it just has to be
 * opened in this browser, where the same-device branch of /auth/landing
 * finishes it directly. Nothing here is on the critical path of signing in.
 *
 * WHY IT SUBMITS A FORM RATHER THAN FETCHING. The finish endpoint answers
 * 303 to the relying party's callback (or to the post-login landing). A
 * fetch would follow that redirect itself and hand us a response body nobody
 * navigates to, stranding the auth code. Submitting the real form makes the
 * 303 an ordinary navigation of the tab that holds the client's PKCE state.
 *
 * Obeys the strict CSP (csp.go): no inline handlers, no eval, no remote
 * sources.
 */

(function () {
  "use strict";

  var POLL_MS = 2000;

  var status = document.getElementById("ml-status");
  var form = document.getElementById("ml-finish");
  if (!status || !form) return;

  var requestId = status.getAttribute("data-request");
  if (!requestId) return;

  // The poll stops at the request's own TTL. An abandoned tab left open
  // overnight must not keep asking about a row that died ten minutes in.
  var windowSeconds = parseInt(status.getAttribute("data-window"), 10);
  if (!isFinite(windowSeconds) || windowSeconds <= 0) windowSeconds = 600;
  var deadline = Date.now() + windowSeconds * 1000;

  var timer = null;
  var finished = false;

  function stop(message, subtle) {
    if (timer) window.clearTimeout(timer);
    timer = null;
    if (message) {
      status.textContent = message;
      if (subtle) status.classList.add("text-subtle");
    }
  }

  function finish() {
    if (finished) return;
    finished = true;
    stop("Signing you in…", false);
    form.submit();
  }

  function tick() {
    if (Date.now() > deadline) {
      stop("This sign-in request has expired. Request a new link to try again.", true);
      return;
    }
    // The memql_ml cookie rides along because the endpoint is under /auth,
    // which is exactly the cookie's Path. same-origin is the default for
    // fetch credentials in every browser we support, but say it, because the
    // whole exchange is worthless without the cookie.
    fetch("/auth/magic-link/status?request=" + encodeURIComponent(requestId), {
      credentials: "same-origin",
      headers: { Accept: "application/json" },
      cache: "no-store",
    })
      .then(function (res) {
        if (res.status === 404) {
          // Unknown id, or this browser does not hold the binding. Both
          // answer the same way on purpose, and neither is recoverable by
          // polling harder.
          stop(null, false);
          return null;
        }
        if (!res.ok) return null;
        return res.json();
      })
      .then(function (body) {
        if (!body) {
          schedule();
          return;
        }
        switch (body.state) {
          case "approved":
            finish();
            return;
          case "consumed":
            stop("This sign-in link has already been used. Request a new one if you still need to sign in.", true);
            return;
          case "expired":
            stop("This sign-in link has expired. Request a new one.", true);
            return;
          default:
            schedule();
        }
      })
      .catch(function () {
        // A transient network failure is not a terminal state; keep asking
        // until the deadline says otherwise.
        schedule();
      });
  }

  function schedule() {
    if (finished) return;
    timer = window.setTimeout(tick, POLL_MS);
  }

  schedule();
})();
