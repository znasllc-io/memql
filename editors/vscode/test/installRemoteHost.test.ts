// A local install refuses a remote window, and says why (memql#4623).
//
// Before this, the install SUCCEEDED under Remote-SSH and every credential
// button then opened a tab that could not connect -- three independent reasons,
// none of them visible: asExternalUri tunnels only loopback authorities so an
// `identity.memql.localhost` URL comes back unchanged; RFC 6761 makes the
// USER's browser resolve the whole .localhost family to their own 127.0.0.1;
// and the mkcert CA went into the REMOTE's trust store.
//
// The README and two source comments all said the flow "falls back
// automatically", so the operator's reasonable conclusion was that MemQL was
// broken.

import test from "node:test";
import assert from "node:assert/strict";

import { localInstallRefusal } from "../src/install/remoteHost.js";

test("a local editor is not refused", () => {
  assert.equal(localInstallRefusal({ remoteName: undefined }), undefined);
  assert.equal(localInstallRefusal({ remoteName: "" }), undefined);
  assert.equal(localInstallRefusal({ remoteName: "   " }), undefined);
});

test("every remote kind is refused, by its own name", () => {
  const cases: Array<[string, RegExp]> = [
    ["ssh-remote+myhost", /Remote-SSH/],
    ["dev-container+abc123", /dev container/],
    ["attached-container+abc123", /dev container/],
    ["codespaces", /Codespaces/],
    ["wsl", /WSL/],
    ["something-new", /remote \(something-new\)/],
  ];
  for (const [remoteName, shape] of cases) {
    const refusal = localInstallRefusal({ remoteName });
    assert.ok(refusal !== undefined, `${remoteName} was allowed to install locally`);
    assert.match(refusal, shape);
  }
});

test("the refusal names the actual cause and a real way forward", () => {
  const refusal = localInstallRefusal({ remoteName: "ssh-remote+box" });
  assert.ok(refusal !== undefined);

  // WHY, in terms an operator can check: the two independent reasons.
  assert.match(refusal, /\.localhost/, "does not explain the name resolution");
  assert.match(refusal, /certificate authority/i, "does not explain the trust store");

  // WHAT INSTEAD. A remote install is a real thing -- from a terminal on that
  // host -- and connecting from here afterwards works. A refusal with no route
  // forward is only half an answer.
  assert.match(refusal, /make up/, "does not name the supported way to install there");
  assert.match(refusal, /Add Cluster/, "does not say connecting still works");

  // And it must not repeat the claim that was wrong.
  assert.ok(
    !/falls back automatically/i.test(refusal),
    "still claims an automatic fallback that does not exist",
  );
});
