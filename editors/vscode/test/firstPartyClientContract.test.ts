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
  WELL_KNOWN_REDIRECT_URI_VSCODE,
} from "../src/auth/wellKnownClient.js";

interface Contract {
  clientId: string;
  clientName: string;
  redirectURI: string;
  redirectURIVSCode: string;
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

test("the extension's vscode:// redirect equals the one identity registers", () => {
  // The SECOND registered redirect (memql#4623), used under Remote-SSH,
  // Codespaces and dev containers. Matched EXACTLY by identity -- RFC 8252
  // §7.3's any-port exception is for loopback and a private-use scheme has no
  // port -- so a one-character drift here strands remote sign-in entirely.
  assert.equal(WELL_KNOWN_REDIRECT_URI_VSCODE, contract.redirectURIVSCode);
  const parsed = new URL(contract.redirectURIVSCode);
  assert.equal(parsed.protocol, "vscode:");
  assert.equal(parsed.pathname, "/callback");
  // The authority is `publisher.name` from package.json. Asserted against the
  // manifest rather than restated, because renaming either half is exactly the
  // change that would silently break this.
  const manifest = JSON.parse(
    fs.readFileSync(path.join(repoRoot(), "editors", "vscode", "package.json"), "utf8"),
  ) as { publisher: string; name: string };
  assert.equal(parsed.hostname, `${manifest.publisher}.${manifest.name}`.toLowerCase());
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
  // TWO registered shapes since memql#4623: the loopback one, whose port
  // varies, and the vscode:// one, which is fixed. Each accepted entry must
  // match ONE of them on scheme, host and path.
  const registered = [contract.redirectURI, contract.redirectURIVSCode].map((u) => new URL(u));
  for (const uri of contract.acceptedCallbacks) {
    const candidate = new URL(uri);
    const matches = registered.some(
      (r) =>
        candidate.protocol === r.protocol &&
        candidate.hostname === r.hostname &&
        candidate.pathname === r.pathname,
    );
    assert.ok(matches, `${uri} is not a shape this extension presents`);
  }
});

test("every callback the fixture says is rejected differs in more than the port", () => {
  // The complement, and the reason it is worth asserting: a "rejected" entry
  // that differed only by port would be testing that the any-port exception
  // does NOT work, which is the opposite of the contract.
  const registered = [contract.redirectURI, contract.redirectURIVSCode].map((u) => new URL(u));
  for (const uri of contract.rejectedCallbacks) {
    const candidate = new URL(uri);
    const differsFromAll = registered.every(
      (r) =>
        candidate.protocol !== r.protocol ||
        candidate.hostname !== r.hostname ||
        candidate.pathname !== r.pathname,
    );
    assert.ok(differsFromAll, `${uri} differs only by port, which the matcher must ACCEPT`);
  }
});
