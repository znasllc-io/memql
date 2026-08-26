// enroll.js -- the browser half of BOTH passkey-registration pages
// (memql#3408 for /enroll, memql#3968 for /recover).
//
// The server has already validated the presented code and rendered the page.
// What is left is the part only a browser can do: the WebAuthn registration
// ceremony, navigator.credentials.create(), running inside the document. That
// is why /auth/webauthn/register/{begin,finish} are HTTP and not gRPC -- there
// is no wire form of "the user touched their security key".
//
// ONE DRIVER FOR TWO ROUTES, PARAMETERISED ON THE AUTHORIZATION SCHEME. The
// enrolment token travels as `Authorization: Enrolment <code>` and the owner
// recovery key as `Authorization: Recovery <code>`; everything else about the
// ceremony -- the base64url helpers, the create() call, the error mapping,
// the single-use-at-finish semantics -- is identical. Forking this file for
// the second route would duplicate the WebAuthn logic on the surface where a
// silent divergence is hardest to notice, so the scheme is read from the
// page's data-auth-scheme attribute and nothing else varies.
//
// The code is read out of location.search rather than echoed into the DOM by
// the server: the value is already in the address bar, and one copy of a
// credential is better than two.
//
// The single-use stamp lands on the FINISH call, server-side, so reloading
// this page or abandoning it halfway does not burn the link.
//
// ENROLMENT ENDS HOLDING A SESSION, NOT AN INSTRUCTION (memql#4610). A
// successful registration used to end at "Passkey created. You can sign in
// with it from now on." -- true, and useless: the credential existed, the
// person had no session, and the next thing asked of them was to go and sign
// in with the key they had produced ten seconds earlier, having already made
// the authenticator gesture that made it. On the invitation path that turns a
// one-click join into a click, a biometric prompt and then a separate sign-in
// (memql#4601); on the local-install path the same tax lands on a brand-new
// owner at the moment they are least sure what is meant to happen next.
//
// So on success this file CHAINS THE LOGIN CEREMONY: navigator.credentials.get()
// against /auth/webauthn/login/{begin,finish}, in this page, and then follows
// the target the server hands back. Two ways to close that gap were on the
// table and this is the cheaper one. The other -- minting a session at
// registration finish -- costs no extra tap but turns the enrolment ceremony
// into a sign-in, and requirePasskeyEnroller documents three authorities that
// authorize REGISTRATION, none of which is stated to authorize a session.
// That is an argument worth making deliberately if it is ever made; it is not
// something an onboarding change should do as a side effect. Chaining costs
// one extra gesture, reuses the login ceremony exactly as it stands, and
// leaves the authorization story alone.
//
// The ceremony is begun as `firstParty: true`, the arm that mints NO auth code
// and produces only the browser session (see handleWebAuthnLoginBegin). This
// page has no relying party to name -- an enrolment link is opened out of an
// email, not by an application -- and asking for the client arm without a
// client is what the server refuses.
//
// /recover gets the same ending for the same reason: an owner who has just
// spent a break-glass key to make a credential is the last person who should
// be asked to go and find the sign-in page.
//
// THE FAILURE PATH IS THE POINT OF MOST OF THE CODE BELOW. Once the finish
// call returns success the passkey EXISTS and the link is SPENT, so every
// subsequent outcome has to be reported as "your passkey was created, and here
// is how to sign in" -- never as a failed registration. A person told
// enrolment failed will open the link again, and the second attempt is refused
// because the first one consumed it. That is a worse place to leave someone
// than the sentence this change replaced.
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
  // The same two shapes for the ASSERTION half (memql#4610)
  //
  // A SECOND COPY OF static/passkey-login.js's decode/encode, which this file
  // otherwise argues against -- so the reason it is a copy rather than a
  // shared module is worth stating. /enroll and /recover load exactly one
  // script (enroll.go and recover.go each pass this file alone in the
  // layout's ExtraScripts), and passkey-login.js is an IIFE that binds itself
  // to the /login page's [data-passkey-login] block on DOMContentLoaded --
  // there is no handle to call, and giving it one, or loading a second script
  // here, is a server-side change on pages another change is already editing.
  //
  // What is copied is also the part that cannot silently diverge: the
  // mechanical base64url shuffle the WebAuthn JSON surface demands, whose
  // failure mode is an assertion the server rejects outright rather than one
  // it accepts differently. Nothing about the ceremony's POLICY lives here --
  // the options come from the server and are passed through untouched, as
  // decodeCreationOptions does above.
  // ---------------------------------------------------------------------

  function decodeRequestOptions(publicKey) {
    publicKey.challenge = fromBase64Url(publicKey.challenge);
    // allowCredentials is empty for this ceremony -- it is usernameless, and
    // the credential is discoverable because registration minted it with
    // residentKey=required. Decoded defensively anyway so a future non-empty
    // list works.
    if (Array.isArray(publicKey.allowCredentials)) {
      publicKey.allowCredentials = publicKey.allowCredentials.map(function (cred) {
        return Object.assign({}, cred, { id: fromBase64Url(cred.id) });
      });
    }
    return publicKey;
  }

  function encodeAssertion(assertion) {
    var response = assertion.response;
    return {
      id: assertion.id,
      rawId: toBase64Url(assertion.rawId),
      type: assertion.type,
      clientExtensionResults:
        typeof assertion.getClientExtensionResults === "function"
          ? assertion.getClientExtensionResults()
          : {},
      response: {
        clientDataJSON: toBase64Url(response.clientDataJSON),
        authenticatorData: toBase64Url(response.authenticatorData),
        signature: toBase64Url(response.signature),
        userHandle: response.userHandle ? toBase64Url(response.userHandle) : "",
      },
    };
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

  // authScheme reads the scheme off the rendered card, defaulting to the
  // enrolment one. A page that somehow rendered without the attribute still
  // works as /enroll did before memql#3968 rather than sending a header the
  // server cannot parse.
  function authScheme() {
    var card = document.querySelector("[data-auth-scheme]");
    if (card === null) {
      return "Enrolment";
    }
    var value = String(card.getAttribute("data-auth-scheme") || "").trim();
    return value === "" ? "Enrolment" : value;
  }

  function send(path, headers, body) {
    return fetch(path, {
      method: "POST",
      credentials: "same-origin",
      headers: Object.assign({ "Content-Type": "application/json" }, headers),
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

  function post(path, code, body) {
    return send(path, { Authorization: authScheme() + " " + code }, body);
  }

  // postUnauthenticated carries NO Authorization header, and that is the
  // point of it existing separately. The login ceremony IS the
  // authentication, so it takes no bearer -- and the enrolment code this page
  // holds has no business travelling to an endpoint that would not read it.
  // A credential sent where it is not needed is a credential in one more
  // access log.
  function postUnauthenticated(path, body) {
    return send(path, {}, body);
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
        // FROM HERE THE PASSKEY EXISTS AND THE LINK IS SPENT. Nothing below
        // may report a failed registration, and nothing below may run the
        // registration ceremony again -- see registrationIsDone().
        registrationIsDone();
        say("Passkey created. Signing you in...");
        return signIn().catch(function (err) {
          offerSignIn(signInProblem(err));
        });
      })
      .catch(function (err) {
        // THE ONE THING THIS CATCH MUST NOT DO is report a failed enrolment
        // after a successful one. Everything from registrationIsDone() on
        // handles its own failures, so reaching here with `registered` set
        // means something unforeseen went wrong AFTER the credential was
        // created -- and the person still has a passkey and a spent link,
        // whatever it was.
        if (registered) {
          offerSignIn(signInProblem(err));
          return;
        }
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

  // ---------------------------------------------------------------------
  // The chained sign-in (memql#4610)
  // ---------------------------------------------------------------------

  // registered flips exactly once, at the moment the server accepts the
  // credential, and never flips back. It is what every branch below reads to
  // decide whether "this failed" is a statement about the registration or
  // about the sign-in.
  var registered = false;

  // spentCredential names what this page was opened with, so the copy after a
  // successful registration reads correctly on BOTH routes this file drives:
  // /enroll consumed an enrolment link, /recover consumed a recovery link,
  // and either way it is spent by the time the sentence is shown. Read off
  // the same data-auth-scheme attribute everything else varies on -- a second
  // literal would be exactly the silent divergence the header warns about.
  function spentCredential() {
    return authScheme() === "Recovery" ? "recovery link" : "enrolment link";
  }

  // signIn runs the passkey LOGIN ceremony and follows the target the server
  // returns. Rejects on every failure so one caller decides what to say.
  //
  // `firstParty: true` asks for the arm that mints no auth code: this page
  // has no relying party to name, and the browser is not an OAuth client. The
  // destination comes back in redirectTo, computed by the server from the
  // cluster domain -- this page never chooses where it lands, which is what
  // lets the server skip the redirect validation the client arm needs.
  function signIn() {
    if (!navigator.credentials.get) {
      return Promise.reject({ handled: "This browser cannot sign in with a passkey." });
    }
    return postUnauthenticated("/auth/webauthn/login/begin", { firstParty: true })
      .then(function (begun) {
        if (!begun.ok || !begun.payload.requestOptions) {
          throw { handled: describe(begun.payload, "Could not start sign-in.") };
        }
        var options = decodeRequestOptions(begun.payload.requestOptions.publicKey);
        return navigator.credentials.get({ publicKey: options }).then(function (assertion) {
          if (!assertion) {
            throw { handled: "No passkey was used." };
          }
          return postUnauthenticated("/auth/webauthn/login/finish", {
            challengeId: begun.payload.challengeId,
            credential: encodeAssertion(assertion),
          });
        });
      })
      .then(function (finished) {
        if (!finished.ok || !finished.payload.redirectTo) {
          throw { handled: describe(finished.payload, "Sign-in did not complete.") };
        }
        window.location.assign(finished.payload.redirectTo);
      });
  }

  // signInProblem renders a chained-sign-in failure as one clause, to be read
  // AFTER "Your passkey was created". Every branch describes the sign-in and
  // never the enrolment, because the enrolment succeeded.
  function signInProblem(err) {
    if (err && err.handled) {
      return err.handled;
    }
    // The expected one, and not a fault. A browser will refuse
    // navigator.credentials.get() with NotAllowedError when the transient
    // user activation from the click that started enrolment has been spent by
    // create() or has since lapsed -- which is likely, because a person spends
    // several seconds at the platform sheet. The Sign in button below is a
    // fresh gesture, so pressing it is expected to work where this did not.
    if (err && err.name === "NotAllowedError") {
      return "The sign-in prompt was dismissed or timed out.";
    }
    return (err && err.message) || "Signing in did not complete.";
  }

  // registrationIsDone closes the registration path for good.
  //
  // The single-use stamp has landed server-side by the time this runs, so a
  // second registration would be refused with enrolment_already_used -- and a
  // person who saw that after a chained sign-in stumbled would reasonably
  // conclude their passkey was never made. The button stops being a
  // registration control here rather than being trusted not to be pressed.
  function registrationIsDone() {
    registered = true;
    startButton.disabled = true;
    startButton.textContent = "Sign in";
  }

  // offerSignIn is the honest end of the failure path: the passkey WAS
  // created, and here are two ways to get in with it. The button retries the
  // ceremony, because the most likely reason it did not run is a lapsed user
  // gesture and a press is a new one; the link is the durable escape for
  // everything else, since /login carries the magic-link form as well.
  function offerSignIn(problem) {
    startButton.disabled = false;
    say(
      "Your passkey was created. " + problem +
        " Press Sign in to try again -- do not open the " + spentCredential() +
        " a second time, it has already been used.",
      "text-success"
    );
    // say() replaced the status line's children, so the link is re-created
    // every time rather than kept -- which is also why a repeated failure
    // cannot accumulate a row of them.
    statusLine.appendChild(document.createTextNode(" "));
    var alternative = document.createElement("a");
    alternative.href = "/login";
    alternative.textContent = "Sign in another way";
    statusLine.appendChild(alternative);
  }

  function retrySignIn() {
    startButton.disabled = true;
    say("Waiting for your device...");
    signIn().catch(function (err) {
      offerSignIn(signInProblem(err));
    });
  }

  // ONE LISTENER, TWO MEANINGS, decided by whether the credential exists yet.
  // Swapping listeners would leave a window in which both are attached; a
  // branch cannot.
  startButton.addEventListener("click", function () {
    if (registered) {
      retrySignIn();
      return;
    }
    run();
  });
})();
