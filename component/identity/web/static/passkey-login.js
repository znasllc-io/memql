/*
 * passkey-login.js
 * ================
 *
 * The "Sign in with a passkey" control on /login (memql#3407).
 *
 * PROGRESSIVE ENHANCEMENT, in the strict sense: the server renders the
 * control hidden, and this script is the only thing that reveals it. So
 * a browser without WebAuthn, a user with no passkey enrolled, or a
 * blocked script all leave the page exactly as it was -- the magic-link
 * form above is untouched and remains the way in. Nothing here is on the
 * critical path of the page working.
 *
 * The OAuth context (client id, redirect URI, state, PKCE challenge)
 * comes off data-* attributes the server rendered from the SAME values
 * it put in the magic-link form's hidden inputs, so both factors carry
 * the identical in-flight context. The server VALIDATES that context at
 * /login/begin and then holds it; this script never gets to restate it
 * at finish time.
 *
 * Obeys the strict CSP (csp.go): no inline handlers, no eval, no remote
 * sources.
 */

(function () {
  "use strict";

  var ROOT = "[data-passkey-login]";

  // base64url <-> ArrayBuffer. The WebAuthn JS API speaks ArrayBuffers
  // and the JSON on the wire speaks base64url (unpadded), which is what
  // go-webauthn's protocol types marshal to and parse from.
  function fromB64url(s) {
    var pad = String(s).replace(/-/g, "+").replace(/_/g, "/");
    while (pad.length % 4) pad += "=";
    var raw = window.atob(pad);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out.buffer;
  }

  function toB64url(buf) {
    var bytes = new Uint8Array(buf);
    var s = "";
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return window.btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        return { ok: res.ok, data: data };
      });
    });
  }

  function wire() {
    var root = document.querySelector(ROOT);
    if (!root) return;

    var button = root.querySelector("[data-passkey-button]");
    var status = root.querySelector("[data-passkey-status]");
    if (!button) return;

    // Feature gate. Without a platform authenticator API there is
    // nothing to reveal, and a visible button that always errors is
    // worse than no button.
    if (!window.PublicKeyCredential || !navigator.credentials || !navigator.credentials.get) {
      return;
    }
    root.hidden = false;

    function fail(message) {
      button.disabled = false;
      button.textContent = button.getAttribute("data-label") || "Sign in with a passkey";
      if (status) {
        status.textContent = message;
        status.hidden = false;
      }
    }

    button.addEventListener("click", function () {
      button.disabled = true;
      button.textContent = button.getAttribute("data-pending-label") || "Waiting for your passkey...";
      if (status) status.hidden = true;

      postJSON("/auth/webauthn/login/begin", {
        clientId: root.getAttribute("data-client-id") || "",
        redirectUri: root.getAttribute("data-redirect-uri") || "",
        state: root.getAttribute("data-state") || "",
        codeChallenge: root.getAttribute("data-code-challenge") || "",
        codeChallengeMethod: root.getAttribute("data-code-challenge-method") || "",
      }).then(function (begin) {
        if (!begin.ok || !begin.data || !begin.data.requestOptions) {
          throw new Error(begin.data && begin.data.error ? begin.data.error : "Could not start passkey sign-in.");
        }
        var options = begin.data.requestOptions.publicKey;
        options.challenge = fromB64url(options.challenge);
        // allowCredentials is empty for this ceremony (usernameless);
        // decode defensively anyway so a future non-empty list works.
        if (options.allowCredentials) {
          options.allowCredentials = options.allowCredentials.map(function (c) {
            return { type: c.type, id: fromB64url(c.id), transports: c.transports };
          });
        }
        return navigator.credentials.get({ publicKey: options }).then(function (assertion) {
          if (!assertion) throw new Error("No passkey was selected.");
          return postJSON("/auth/webauthn/login/finish", {
            challengeId: begin.data.challengeId,
            credential: {
              id: assertion.id,
              rawId: toB64url(assertion.rawId),
              type: assertion.type,
              clientExtensionResults: assertion.getClientExtensionResults ? assertion.getClientExtensionResults() : {},
              response: {
                clientDataJSON: toB64url(assertion.response.clientDataJSON),
                authenticatorData: toB64url(assertion.response.authenticatorData),
                signature: toB64url(assertion.response.signature),
                userHandle: assertion.response.userHandle ? toB64url(assertion.response.userHandle) : "",
              },
            },
          });
        });
      }).then(function (finish) {
        if (!finish.ok || !finish.data || !finish.data.redirectTo) {
          throw new Error(finish.data && finish.data.error ? finish.data.error : "Passkey sign-in failed.");
        }
        window.location.assign(finish.data.redirectTo);
      }).catch(function (err) {
        // NotAllowedError is what the browser reports for "the user
        // dismissed the sheet" AND for "no credential matched", and it
        // is not worth surfacing the distinction the browser refuses to
        // draw. Everything else shows its message.
        if (err && err.name === "NotAllowedError") {
          fail("No passkey was used. You can still sign in with a link.");
          return;
        }
        fail((err && err.message) || "Passkey sign-in failed. You can still sign in with a link.");
      });
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
