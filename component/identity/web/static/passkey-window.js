/*
 * passkey-window.js
 * =================
 *
 * Identity's passkey window (memql#6233). MemQL OS opens this page when the
 * browser refuses its related-origin passkey request, hands it the request by
 * postMessage, and gets back where to go next. Here, on identity's own
 * origin, the RP ID is the page's own host, so begin, the prompt and finish
 * need nothing from the browser that it could refuse.
 *
 * A request is accepted only from the window that opened this one, and only
 * from an origin the server rendered into data-opener-origins (the installed
 * MemQL OS); the result goes back to that origin alone. The request carries
 * the same authorization OS would have sent itself -- by message, never in a
 * URL, so nothing lands in history or a log.
 *
 * The prompt waits for a press here: a browser grants passkey prompts to a
 * person's gesture, and the press that opened this window happened in OS. The
 * options are fetched as soon as the request arrives, so nothing stands
 * between that press and the prompt.
 *
 * Obeys the strict CSP (csp.go): no inline handlers, no eval, no remote
 * sources.
 */

(function () {
  "use strict";

  var root = document.querySelector("[data-passkey-window]");
  if (!root) return;
  var heading = root.querySelector("[data-passkey-heading]");
  var status = root.querySelector("[data-passkey-status]");
  var button = root.querySelector("[data-passkey-act]");
  var openers = (root.getAttribute("data-opener-origins") || "").split(" ").filter(Boolean);
  var opener = window.opener;
  if (!heading || !status || !button || !opener || !openers.length) return;

  var request = null; // what OS asked for
  var origin = "";    // the OS origin it came from
  var begun = null;   // the identity's begin answer, ready for the press

  function say(text) { status.textContent = text; }

  function tell(message) { opener.postMessage(message, origin); }

  function post(path, body) {
    var headers = { "Content-Type": "application/json" };
    if (request.authorization) headers.Authorization = request.authorization;
    return fetch(path, { method: "POST", credentials: "same-origin", headers: headers, body: JSON.stringify(body) })
      .then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (data) {
          if (!res.ok) throw new Error(data.error || "The passkey request failed.");
          return data;
        });
      });
  }

  // Where the browser looks first, as OS would have asked
  // (placeCreationOptions in clients/os/src/auth/passkeys.ts).
  function place(publicKey, where) {
    var selection = Object.assign({}, publicKey.authenticatorSelection || {});
    if (where === "device") {
      selection.authenticatorAttachment = "platform";
      return Object.assign({}, publicKey, { authenticatorSelection: selection, hints: ["client-device"] });
    }
    delete selection.authenticatorAttachment;
    return Object.assign({}, publicKey, { authenticatorSelection: selection, hints: ["hybrid", "security-key"] });
  }

  function deviceFirst() {
    var check = window.PublicKeyCredential && PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable;
    if (typeof check !== "function") return Promise.resolve(false);
    return check.call(PublicKeyCredential).catch(function () { return false; });
  }

  function done(redirectTo) {
    say("Done. You can close this window.");
    button.hidden = true;
    tell({ type: "memql-passkey:done", redirectTo: redirectTo || "" });
    window.close();
  }

  function refuse(error) {
    tell({ type: "memql-passkey:failed", error: error && error.message ? error.message : "The passkey request failed." });
    window.close();
  }

  // Begin, ahead of the press. A register that needs no prompt (already
  // resumed or redirected by identity) finishes here.
  function prepare() {
    begun = null;
    button.disabled = true;
    var path = request.kind === "register" ? "/auth/webauthn/register/begin" : "/auth/webauthn/login/begin";
    var body = request.kind === "register" ? { label: request.label } : request.begin;
    return post(path, body).then(function (begin) {
      if (request.kind === "register" && begin.redirectTo) return done(begin.redirectTo);
      if (request.kind === "register" && begin.resume) {
        return post("/auth/webauthn/register/finish", {}).then(function (finish) { done(finish.redirectTo); });
      }
      if (request.kind === "login") {
        begun = { challengeId: begin.challengeId, publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(begin.requestOptions.publicKey) };
        button.disabled = false;
        return;
      }
      var chosen = request.place ? Promise.resolve(request.place) : deviceFirst().then(function (yes) { return yes ? "device" : ""; });
      return chosen.then(function (where) {
        var publicKey = where ? place(begin.creationOptions.publicKey, where) : begin.creationOptions.publicKey;
        begun = { challengeId: begin.challengeId, publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(publicKey) };
        button.disabled = false;
      });
    }).catch(refuse);
  }

  function press() {
    if (!begun) return;
    button.disabled = true;
    var registering = request.kind === "register";
    var challengeId = begun.challengeId;
    var asked = registering ? navigator.credentials.create({ publicKey: begun.publicKey }) : navigator.credentials.get({ publicKey: begun.publicKey });
    asked.then(function (credential) {
      if (!credential) throw new Error(registering ? "No passkey was created." : "No passkey was selected.");
      say("Finishing...");
      return registering
        ? post("/auth/webauthn/register/finish", { challengeId: challengeId, label: request.label, credential: credential.toJSON() })
        : post("/auth/webauthn/login/finish", { challengeId: challengeId, credential: credential.toJSON() });
    }).then(function (finish) {
      done(finish.redirectTo);
    }).catch(function (error) {
      // A cancelled prompt is the person's answer: they may press again.
      say(error && error.name === "NotAllowedError" ? "No passkey was used. Press the button to try again." : (error && error.message) || "The passkey request failed.");
      prepare();
    });
  }

  window.addEventListener("message", function (event) {
    if (event.source !== opener || openers.indexOf(event.origin) < 0) return;
    var message = event.data;
    if (!message || message.type !== "memql-passkey:request" || request) return;
    if (message.kind !== "register" && message.kind !== "login") return;
    request = message;
    origin = event.origin;
    var registering = request.kind === "register";
    heading.textContent = registering ? "Create your passkey" : "Sign in with your passkey";
    button.textContent = registering ? "Create passkey" : "Sign in";
    button.hidden = false;
    say("Your browser will ask you to confirm.");
    prepare();
  });
  button.addEventListener("click", press);

  // OS waits for this before it sends the request.
  for (var i = 0; i < openers.length; i++) opener.postMessage({ type: "memql-passkey:ready" }, openers[i]);
})();
