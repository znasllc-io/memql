/*
 * me-passkeys.js
 * ==============
 *
 * Drives the "Add a passkey" button on /me/devices against the
 * registration ceremony from memql#3406
 * (POST /auth/webauthn/register/{begin,finish}).
 *
 * This is the ONE thing on the passkey card that cannot be a form post:
 * navigator.credentials.create() is a browser API, so the bytes the
 * ceremony needs only exist after JavaScript has run. Everything else on
 * the card -- listing, rename, revoke, and the last-credential warning --
 * is server-rendered precisely so that none of it depends on this file
 * loading.
 *
 * No framework: the page's other behaviour is plain progressive
 * enhancement (app.js), and there are no Stimulus controllers in this
 * app yet, so this follows the same shape -- data attributes, one
 * listener, wired on DOM ready. It obeys the strict CSP (no inline
 * handlers, no eval, no remote sources).
 *
 * The bearer token: the ceremony endpoints take Authorization: Bearer,
 * and the page's session cookie is HttpOnly, so the token is minted at
 * CLICK TIME from POST /auth/refresh rather than read from
 * IdentityMe.accessToken. Two reasons, and the second is the real one:
 * a token fetched at page load can be minutes stale by the time somebody
 * gets round to enrolling, and the click is a user gesture -- the one
 * moment a WebAuthn ceremony is allowed to start anyway.
 */

(function () {
  "use strict";

  var ADD = "[data-passkeys-add]";
  var STATUS = "[data-passkeys-status]";

  ready(function () {
    var button = document.querySelector(ADD);
    if (!button) return;

    if (!supported()) {
      // Say what is wrong rather than failing on click. A browser
      // without WebAuthn is not a broken page; it is a browser that
      // cannot do this, and the user's email sign-in still works.
      button.disabled = true;
      say("This browser doesn't support passkeys. You can still sign in by email.", "text-muted");
      return;
    }
    button.addEventListener("click", function () {
      registerPasskey(button);
    });
  });

  function ready(fn) {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", fn, { once: true });
    } else {
      fn();
    }
  }

  function say(message, cls) {
    var zone = document.querySelector(STATUS);
    if (!zone) return;
    zone.className = "text-small " + (cls || "text-muted");
    zone.textContent = message;
  }

  function registerPasskey(button) {
    button.disabled = true;
    say("Waiting for your device\u2026");

    accessToken()
      .then(function (token) {
        return begin(token).then(function (started) {
          return create(started.creationOptions).then(function (credential) {
            return finish(token, started.challengeId, credential);
          });
        });
      })
      .then(function (result) {
        // Reload rather than splice a row in: the server owns the list,
        // the backup posture and the sign-in-route sentence, and a
        // client-side row would be a second opinion about all three.
        window.location.replace("/me/devices?flash=" +
          encodeURIComponent("Passkey added: " + (result.label || "unnamed")) +
          "&flash_kind=success");
      })
      .catch(function (err) {
        button.disabled = false;
        say(message(err), "text-danger");
      });
  }

  function supported() {
    return !!(window.PublicKeyCredential && navigator.credentials && navigator.credentials.create);
  }

  // accessToken exchanges the identity-domain session cookie for a
  // short-lived bearer. A 401 here means the session is gone, which is
  // the same answer the page shell gives: go and sign in again.
  function accessToken() {
    return fetch("/auth/refresh", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    }).then(function (resp) {
      if (!resp.ok) throw named("session_expired", "Your session expired. Sign in again, then add the passkey.");
      return resp.json();
    }).then(function (data) {
      if (!data || !data.access_token) {
        throw named("session_expired", "Your session expired. Sign in again, then add the passkey.");
      }
      return data.access_token;
    });
  }

  function begin(token) {
    return postJSON("/auth/webauthn/register/begin", token, {}).then(function (data) {
      if (!data.success || !data.creationOptions || !data.challengeId) {
        throw named(data.errorCode || "begin_failed", data.error || "Could not start registration.");
      }
      return data;
    });
  }

  function finish(token, challengeId, credential) {
    return postJSON("/auth/webauthn/register/finish", token, {
      challengeId: challengeId,
      credential: credential,
    }).then(function (data) {
      if (!data.success) {
        throw named(data.errorCode || "finish_failed", data.error || "Could not complete registration.");
      }
      return data;
    });
  }

  function postJSON(path, token, body) {
    return fetch(path, {
      method: "POST",
      credentials: "include",
      headers: {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "Authorization": "Bearer " + token,
      },
      body: JSON.stringify(body),
    }).then(function (resp) {
      return resp.json().catch(function () {
        throw named("http_" + resp.status, "The server returned an unreadable response.");
      });
    });
  }

  // create runs the ceremony. The server hands back the options exactly
  // as navigator.credentials.create() wants them EXCEPT for the binary
  // fields, which travel as base64url over JSON and have to be decoded
  // back into ArrayBuffers here.
  function create(options) {
    var pk = options.publicKey || options;
    var request = Object.assign({}, pk, {
      challenge: fromBase64Url(pk.challenge),
      user: Object.assign({}, pk.user, { id: fromBase64Url(pk.user.id) }),
    });
    if (Array.isArray(pk.excludeCredentials)) {
      request.excludeCredentials = pk.excludeCredentials.map(function (c) {
        return Object.assign({}, c, { id: fromBase64Url(c.id) });
      });
    }
    return navigator.credentials.create({ publicKey: request }).then(function (credential) {
      if (!credential) throw named("cancelled", "No passkey was created.");
      return {
        id: credential.id,
        rawId: toBase64Url(credential.rawId),
        type: credential.type,
        clientExtensionResults: credential.getClientExtensionResults
          ? credential.getClientExtensionResults()
          : {},
        response: {
          clientDataJSON: toBase64Url(credential.response.clientDataJSON),
          attestationObject: toBase64Url(credential.response.attestationObject),
          transports: credential.response.getTransports
            ? credential.response.getTransports()
            : [],
        },
      };
    });
  }

  // message turns a failure into something worth reading. The two cases
  // that get their own sentence are the two that are not errors at all:
  // the user changed their mind, and the authenticator is already
  // registered.
  function message(err) {
    if (!err) return "Something went wrong. Please try again.";
    if (err.name === "NotAllowedError" || err.code === "cancelled") {
      return "No passkey was added — the request was dismissed or timed out.";
    }
    if (err.name === "InvalidStateError" || err.code === "duplicate_credential") {
      return "That authenticator is already registered on this account.";
    }
    return err.message || "Something went wrong. Please try again.";
  }

  function named(code, msg) {
    var e = new Error(msg);
    e.code = code;
    return e;
  }

  function fromBase64Url(value) {
    if (value instanceof ArrayBuffer) return value;
    var s = String(value).replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var raw = window.atob(s);
    var bytes = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
    return bytes.buffer;
  }

  function toBase64Url(buffer) {
    var bytes = new Uint8Array(buffer);
    var s = "";
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return window.btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
})();
