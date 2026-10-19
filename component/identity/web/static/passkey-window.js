/*
 * passkey-window.js
 * =================
 *
 * Identity's passkey window (memql#6233). MemQL OS opens this page when the
 * browser refuses its related-origin passkey request. Here, on identity's own
 * origin, the RP ID is the page's own host, so the browser's prompt has
 * nothing to refuse.
 *
 * ONLY THE PROMPT RUNS HERE. MemQL OS keeps begin and finish on its own
 * origin, where identity's host-only cookies live, and hands this window the
 * public creation or request options; the window answers with the credential.
 * It makes no request of its own and never sees a token.
 *
 * A request is accepted only from the window that opened this one, and only
 * from an origin the server rendered into data-opener-origins (the installed
 * MemQL OS); the answer goes back to that origin alone.
 *
 * The prompt waits for a press here: a browser grants passkey prompts to a
 * person's gesture, and the press that opened this window happened in OS.
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
  if (!heading || !status || !button || !opener || !openers.length || !window.PublicKeyCredential) return;

  var origin = "";  // the OS origin the request came from
  var kind = "";    // "create" or "get"
  var options = null;

  function say(text) { status.textContent = text; }

  function press() {
    if (!options) return;
    button.disabled = true;
    var asked = kind === "create" ? navigator.credentials.create({ publicKey: options }) : navigator.credentials.get({ publicKey: options });
    asked.then(function (credential) {
      if (!credential) throw new DOMException(kind === "create" ? "No passkey was created." : "No passkey was selected.", "NotAllowedError");
      say("Finishing in MemQL...");
      button.hidden = true;
      opener.postMessage({ type: "memql-passkey:credential", credential: credential.toJSON() }, origin);
    }).catch(function (error) {
      if (error && error.name === "NotAllowedError") {
        // A cancelled prompt is the person's answer: they may press again.
        say("No passkey was used. Press the button to try again.");
        button.disabled = false;
        return;
      }
      opener.postMessage({ type: "memql-passkey:failed", name: (error && error.name) || "Error", error: (error && error.message) || "The passkey request failed." }, origin);
    });
  }

  window.addEventListener("message", function (event) {
    if (event.source !== opener || openers.indexOf(event.origin) < 0) return;
    var message = event.data;
    if (!message || message.type !== "memql-passkey:request" || options) return;
    if (message.kind !== "create" && message.kind !== "get") return;
    try {
      options = message.kind === "create"
        ? PublicKeyCredential.parseCreationOptionsFromJSON(message.publicKey)
        : PublicKeyCredential.parseRequestOptionsFromJSON(message.publicKey);
    } catch (error) {
      opener.postMessage({ type: "memql-passkey:failed", name: "NotSupportedError", error: "This browser could not read the passkey request." }, event.origin);
      return;
    }
    origin = event.origin;
    kind = message.kind;
    heading.textContent = kind === "create" ? "Create your passkey" : "Sign in with your passkey";
    button.textContent = kind === "create" ? "Create passkey" : "Sign in";
    button.hidden = false;
    button.disabled = false;
    say("Your browser will ask you to confirm.");
  });
  button.addEventListener("click", press);

  // OS waits for this before it sends the request.
  for (var i = 0; i < openers.length; i++) opener.postMessage({ type: "memql-passkey:ready" }, openers[i]);
})();
