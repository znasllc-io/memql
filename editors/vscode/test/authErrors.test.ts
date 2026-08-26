// What a network failure actually SAYS (memql#4619).
//
// The extension targets Node 20 (esbuild.js), whose global `fetch` is undici,
// and undici reports every transport failure identically: it throws
// `TypeError: fetch failed` and puts the reason -- the DNS, socket or TLS error
// -- in `.cause`. A renderer that read `err.message` alone therefore reduced a
// wrong hostname, a firewall, an expired certificate and an unknown CA to one
// sentence naming none of them, on every sign-in surface at once.
//
// These tests build the exact shapes undici throws, because the SHAPE is the
// whole defect: nothing in src/auth/ was exercised against a throwing fetch at
// all, so a renderer that never looked past the top-level message passed every
// test in this tree while telling the operator nothing.

import test from "node:test";
import assert from "node:assert/strict";

import { errorText } from "../src/auth/errors.js";

/** The undici wrapper: its message is always this, and the truth is in `.cause`. */
function fetchFailed(cause: unknown): TypeError {
  const err = new TypeError("fetch failed");
  (err as { cause?: unknown }).cause = cause;
  return err;
}

/** A Node system error: a message and, crucially, a `code`. */
function systemError(message: string, code: string): Error {
  const err = new Error(message);
  (err as { code?: string }).code = code;
  return err;
}

// -----------------------------------------------------------------------------
// The transport codes: four problems that used to share one sentence
// -----------------------------------------------------------------------------

test("a wrong hostname reports ENOTFOUND and the name that did not resolve", () => {
  const text = errorText(
    fetchFailed(systemError("getaddrinfo ENOTFOUND identity.example.com", "ENOTFOUND")),
  );

  // The code is NOT repeated: the system message already names it, and
  // "ENOTFOUND -- getaddrinfo ENOTFOUND ..." is noise an operator has to read
  // past. What must survive is the code and the name that failed to resolve.
  assert.equal(text, "fetch failed: getaddrinfo ENOTFOUND identity.example.com");
});

test("a refused connection reports ECONNREFUSED out of undici's AggregateError", () => {
  // The common shape when a local cluster is simply not running: a host that
  // resolves to several addresses fails on each, and undici hands back an
  // AggregateError whose OWN message is empty. A walk that followed `.cause`
  // only would stop on that empty message and report nothing at all.
  const everyAddress = new AggregateError(
    [
      systemError("connect ECONNREFUSED 127.0.0.1:443", "ECONNREFUSED"),
      systemError("connect ECONNREFUSED ::1:443", "ECONNREFUSED"),
    ],
    "",
  );

  assert.equal(
    errorText(fetchFailed(everyAddress)),
    "fetch failed: connect ECONNREFUSED 127.0.0.1:443",
  );
});

test("an unknown CA explains that Node does not read the OS trust store", () => {
  // The single most confusing case, and the reason the TLS codes are special
  // -cased at all: the fix is not guessable from the code. The operator's
  // browser and curl trust their mkcert root, so the certificate looks fine
  // everywhere they can check it, and only the editor objects.
  const text = errorText(
    fetchFailed(
      systemError("unable to verify the first certificate", "UNABLE_TO_VERIFY_LEAF_SIGNATURE"),
    ),
  );

  assert.match(text, /UNABLE_TO_VERIFY_LEAF_SIGNATURE/);
  assert.match(text, /unable to verify the first certificate/);
  assert.match(text, /trust store/);
  assert.match(text, /NODE_EXTRA_CA_CERTS/);
});

test("a self-signed certificate gets the same trust-store explanation", () => {
  const text = errorText(
    fetchFailed(systemError("self-signed certificate", "DEPTH_ZERO_SELF_SIGNED_CERT")),
  );

  assert.match(text, /DEPTH_ZERO_SELF_SIGNED_CERT/);
  assert.match(text, /NODE_EXTRA_CA_CERTS/);
});

test("an expired certificate is told to be reissued, NOT to be trusted harder", () => {
  // A different problem with a different fix. Node checks the date itself, so
  // pointing NODE_EXTRA_CA_CERTS at the CA changes nothing -- offering that
  // here would send the operator to edit a setting that cannot help.
  const text = errorText(fetchFailed(systemError("certificate has expired", "CERT_HAS_EXPIRED")));

  assert.match(text, /CERT_HAS_EXPIRED/);
  assert.match(text, /reissue/i);
  assert.doesNotMatch(text, /NODE_EXTRA_CA_CERTS/);
});

test("a certificate for the wrong name points at the hostname, not at trust", () => {
  const text = errorText(
    fetchFailed(
      systemError(
        "Hostname/IP does not match certificate's altnames: Host: api.memql.localhost. is not in the cert's altnames: DNS:memql.localhost",
        "ERR_TLS_CERT_ALTNAME_INVALID",
      ),
    ),
  );

  assert.match(text, /ERR_TLS_CERT_ALTNAME_INVALID/);
  assert.match(text, /hostname/i);
  assert.doesNotMatch(text, /NODE_EXTRA_CA_CERTS/);
});

// -----------------------------------------------------------------------------
// The shapes that must NOT change: this renderer is on ~a dozen call sites
// -----------------------------------------------------------------------------

test("an ordinary Error is rendered exactly as before", () => {
  assert.equal(errorText(new Error("spawn xdg-open ENOENT")), "spawn xdg-open ENOENT");
  assert.equal(errorText("no external URI resolver on this host"), "no external URI resolver on this host");
  assert.equal(errorText(undefined), "undefined");
});

test("a system error thrown WITHOUT a wrapper gains its code once, not twice", () => {
  const text = errorText(
    systemError("unable to verify the first certificate", "UNABLE_TO_VERIFY_LEAF_SIGNATURE"),
  );

  // The message is already the head, so only the code and the advice are added.
  assert.match(text, /^unable to verify the first certificate \(UNABLE_TO_VERIFY_LEAF_SIGNATURE\)\./);
  assert.match(text, /NODE_EXTRA_CA_CERTS/);
});

test("a wrapped cause with no code still surfaces its message", () => {
  assert.equal(errorText(fetchFailed(new Error("socket hang up"))), "fetch failed: socket hang up");
});

test("a cause that points back at itself terminates instead of spinning", () => {
  const looping = new Error("outer");
  (looping as { cause?: unknown }).cause = looping;

  assert.equal(errorText(looping), "outer");
});
