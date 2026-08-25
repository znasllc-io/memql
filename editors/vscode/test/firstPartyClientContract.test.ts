// The TypeScript half of the first-party-client contract (memql#4515).
//
// identity carries the editor compiled in
// (component/identity/builtin_clients.go); this extension authorizes as it
// without registering (src/auth/wellKnownClient.ts). Nothing in the build links
// the two -- separate languages, separate modules, separate release artifacts,
// each individually tested and green.
//
// So the constants they must agree on are written down once, in
// test/fixtures/first-party-client-contract.json, and asserted from both sides.
// The Go half is component/identity/first_party_client_contract_test.go and
// reads the same file.
//
// A disagreement is invisible until a released extension meets a released
// cluster, and it presents as "Unknown client" on the consent page or
// invalid_client on /device/code -- which reads exactly like the 403
// registration_disabled failure this epic removed, and would send the next
// reader down the same wrong path (memql#4514).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import {
  WELL_KNOWN_CLIENT_ID,
  WELL_KNOWN_REDIRECT_URI,
} from "../src/auth/wellKnownClient.js";

interface Contract {
  clientId: string;
  clientName: string;
  redirectURI: string;
  minRole: string;
  acceptedCallbacks: string[];
  rejectedCallbacks: string[];
}

// Walk up to the repository root by the go.work marker. This runs from
// dist-test/ (esbuild output) rather than from test/, so a hard-coded depth
// silently reads the wrong tree the moment either layout moves.
function repoRoot(): string {
  let dir = __dirname;
  for (;;) {
    if (fs.existsSync(path.join(dir, "go.work"))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) {
      throw new Error(`could not locate the repository root (no go.work above ${__dirname})`);
    }
    dir = parent;
  }
}

const contract = JSON.parse(
  fs.readFileSync(
    path.join(repoRoot(), "test", "fixtures", "first-party-client-contract.json"),
    "utf8",
  ),
) as Contract;

test("the extension's client id equals the one identity carries", () => {
  assert.equal(WELL_KNOWN_CLIENT_ID, contract.clientId);
});

test("the extension's redirect URI equals the one identity registers", () => {
  assert.equal(WELL_KNOWN_REDIRECT_URI, contract.redirectURI);
});

test("the contract's redirect URI is portless", () => {
  // RFC 8252 §7.3's any-port exception applies ONLY to a registered loopback
  // URI with no explicit port. Adding one here would opt the built-in back
  // into exact-match and break every callback -- on the SECOND sign-in, since
  // the first would still be running against whatever port it happened to get.
  const parsed = new URL(contract.redirectURI);
  assert.equal(parsed.port, "");
  assert.equal(parsed.hostname, "127.0.0.1");
  assert.equal(parsed.pathname, "/callback");
});

test("every callback the fixture says is accepted is one this extension could present", () => {
  // The Go half proves identity's matcher ACCEPTS these. This half proves they
  // are the shapes this extension actually produces: the loopback listener
  // varies the port and nothing else, so scheme, host and path must equal the
  // registered URI on every one of them.
  const registered = new URL(contract.redirectURI);
  for (const uri of contract.acceptedCallbacks) {
    const candidate = new URL(uri);
    assert.equal(candidate.protocol, registered.protocol, uri);
    assert.equal(candidate.hostname, registered.hostname, uri);
    assert.equal(candidate.pathname, registered.pathname, uri);
  }
});

test("every callback the fixture says is rejected differs in more than the port", () => {
  // The complement, and the reason it is worth asserting: a "rejected" entry
  // that differed only by port would be testing that the any-port exception
  // does NOT work, which is the opposite of the contract.
  const registered = new URL(contract.redirectURI);
  for (const uri of contract.rejectedCallbacks) {
    const candidate = new URL(uri);
    const differs =
      candidate.protocol !== registered.protocol ||
      candidate.hostname !== registered.hostname ||
      candidate.pathname !== registered.pathname;
    assert.ok(differs, `${uri} differs only by port, which the matcher must ACCEPT`);
  }
});
