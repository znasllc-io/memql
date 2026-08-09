// enroll.js -- the browser half of the enrolment page (memql#3408).
//
// GET /enroll?code=mql_enr_... has already validated the token server-side and
// rendered this page. What is left is the part only a browser can do: the
// WebAuthn registration ceremony, navigator.credentials.create(), running
// inside the document. That is why /auth/webauthn/register/{begin,finish} are
// HTTP and not gRPC -- there is no wire form of "the user touched their
// security key".
//
// The token travels as `Authorization: Enrolment <code>`, exactly the way the
// worker-pairing redeem sends `Authorization: Pair <code>`. It is read out of
// location.search rather than echoed into the DOM by the server: the value is
// already in the address bar, and one copy of a credential is better than two.
//
// The single-use stamp lands on the FINISH call, server-side, so reloading
// this page or abandoning it halfway does not burn the link.
//
// No framework: this file is loaded by the layout's ExtraScripts with `defer`,
// under a `script-src 'self'` CSP that forbids inline script.
(function () {
  "use strict";

  var startButton = document.getElementById("enroll-start");
  var statusLine = document.getElementById("enroll-status");
  if (startButton === null || statusLine === null) {
    // A rejection page. Nothing to drive.
    return;
  }

  // ---------------------------------------------------------------------
  // base64url <-> ArrayBuffer
  //
  // The WebAuthn JSON surface is base64url with no padding on the way in AND
  // on the way out: the server's protocol.URLEncodedBase64 emits it, and the
  // Go library parses the attestation back out of it. Plain btoa/atob are
  // base64, so `+` and `/` would survive into a URL-safe field and the server
  // would reject bytes the browser produced correctly.
  // ---------------------------------------------------------------------

  function fromBase64Url(value) {
    var padded = String(value).replace(/-/g, "+").replace(/_/g, "/");
    while (padded.length % 4 !== 0) {
      padded += "=";
    }
    var raw = window.atob(padded);
    var bytes = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) {
      bytes[i] = raw.charCodeAt(i);
    }
    return bytes.buffer;
  }

  function toBase64Url(buffer) {
    var bytes = new Uint8Array(buffer);
    var binary = "";
    for (var i = 0; i < bytes.length; i++) {
      binary += String.fromCharCode(bytes[i]);
    }
    return window.btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  // decodeCreationOptions rewrites the server's base64url strings into the
  // ArrayBuffers navigator.credentials.create() requires. Everything else on
  // the options object is passed through untouched -- the server decides the
  // ceremony's parameters and the page does not get a vote.
  function decodeCreationOptions(publicKey) {
    publicKey.challenge = fromBase64Url(publicKey.challenge);
    if (publicKey.user && typeof publicKey.user.id === "string") {
      publicKey.user.id = fromBase64Url(publicKey.user.id);
    }
    if (Array.isArray(publicKey.excludeCredentials)) {
      publicKey.excludeCredentials = publicKey.excludeCredentials.map(function (cred) {
        return Object.assign({}, cred, { id: fromBase64Url(cred.id) });
      });
    }
    return publicKey;
  }

  // encodeCredential produces the JSON shape the Go WebAuthn library parses.
  function encodeCredential(credential) {
    var response = credential.response;
    var out = {
      id: credential.id,
      rawId: toBase64Url(credential.rawId),
      type: credential.type,
      clientExtensionResults:
        typeof credential.getClientExtensionResults === "function"
          ? credential.getClientExtensionResults()
          : {},
      response: {
        clientDataJSON: toBase64Url(response.clientDataJSON),
        attestationObject: toBase64Url(response.attestationObject),
      },
    };
    if (typeof response.getTransports === "function") {
      // A HINT for the login ceremony's allowCredentials, not a gate. An
      // authenticator that does not implement getTransports is fine.
      out.response.transports = response.getTransports();
    }
    if (credential.authenticatorAttachment) {
      out.authenticatorAttachment = credential.authenticatorAttachment;
    }
    return out;
  }

  // ---------------------------------------------------------------------
  // Status reporting
  // ---------------------------------------------------------------------

  function say(text, kind) {
    statusLine.textContent = text;
    statusLine.className = "text-small mt-2" + (kind ? " " + kind : "");
  }

  // MESSAGES ARE PER-STATE, NOT GENERIC. The server answers a spent, expired
  // or revoked token with its own errorCode, and each one means something
  // different for what the holder should do next. Collapsing them into "could
  // not register" would throw that away at the last step, after the page went
  // to the trouble of distinguishing them at the first.
  var REJECTIONS = {
    enrolment_invalid: "This enrolment link is not valid. Ask for a new one.",
    enrolment_expired: "This enrolment link expired before it was used. Ask for a new one.",
    enrolment_already_used:
      "This enrolment link has already been used. If that was not you, tell whoever issued it.",
    enrolment_revoked: "This enrolment link was revoked. Ask whoever issued it for another.",
    duplicate_credential: "This device already has a passkey on this cluster. Try signing in with it.",
    rate_limited: "Too many attempts from this network. Wait a few minutes and try again.",
    insecure_transport: "Enrolment requires a secure (https) connection.",
  };

  function describe(payload, fallback) {
    if (payload && payload.errorCode && REJECTIONS[payload.errorCode]) {
      return REJECTIONS[payload.errorCode];
    }
    if (payload && payload.error) {
      return payload.error;
    }
    return fallback;
  }

  // ---------------------------------------------------------------------
  // The ceremony
  // ---------------------------------------------------------------------

  function enrolmentCode() {
    try {
      return new URLSearchParams(window.location.search).get("code") || "";
    } catch (err) {
      return "";
    }
  }

  function post(path, code, body) {
    return fetch(path, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        "Content-Type": "application/json",
        Authorization: "Enrolment " + code,
      },
      body: JSON.stringify(body),
    }).then(function (res) {
      return res
        .json()
        .catch(function () {
          return {};
        })
        .then(function (payload) {
          return { ok: res.ok, payload: payload };
        });
    });
  }

  function run() {
    var code = enrolmentCode();
    if (code === "") {
      say(describe(null, "This page needs the enrolment link it was opened with."), "text-error");
      return;
    }
    if (!window.PublicKeyCredential || !navigator.credentials) {
      say("This browser cannot create passkeys. Try a current version of Chrome, Edge, Safari or Firefox.", "text-error");
      return;
    }

    startButton.disabled = true;
    say("Waiting for your device...");

    post("/auth/webauthn/register/begin", code, {})
      .then(function (begun) {
        if (!begun.ok || !begun.payload.creationOptions) {
          throw { handled: describe(begun.payload, "Could not start passkey setup.") };
        }
        var options = decodeCreationOptions(begun.payload.creationOptions.publicKey);
        return navigator.credentials.create({ publicKey: options }).then(function (credential) {
          if (credential === null) {
            throw { handled: "Your device did not produce a passkey. Try again." };
          }
          return post("/auth/webauthn/register/finish", code, {
            challengeId: begun.payload.challengeId,
            credential: encodeCredential(credential),
          });
        });
      })
      .then(function (finished) {
        if (!finished.ok || finished.payload.success !== true) {
          throw { handled: describe(finished.payload, "Your device made a passkey but the server would not accept it.") };
        }
        say("Passkey created. You can sign in with it from now on.", "text-success");
        startButton.textContent = "Done";
      })
      .catch(function (err) {
        startButton.disabled = false;
        if (err && err.handled) {
          say(err.handled, "text-error");
          return;
        }
        // NotAllowedError is what a browser reports both for "the user
        // cancelled" and for "the ceremony timed out", and it is by far the
        // most common outcome that is not a fault. Saying "cancelled" for a
        // timeout is a smaller lie than reporting a raw DOMException name.
        if (err && err.name === "NotAllowedError") {
          say("Passkey setup was cancelled. The link is still good -- try again.");
          return;
        }
        if (err && err.name === "InvalidStateError") {
          say(REJECTIONS.duplicate_credential, "text-error");
          return;
        }
        say((err && err.message) || "Passkey setup failed.", "text-error");
      });
  }

  startButton.addEventListener("click", run);
})();
