// Role wire-mapping: every value the proto's UserRole declares must survive
// the trip into the SDK's `Role` (memql#3331).
//
// THE BUG THIS PINS. `UserRoleWire` stopped at READER while memql.proto
// defined USER_ROLE_DEVELOPER = 5, so roleFromWire's `?? ""` fallback turned a
// developer into an indeterminate role. Nothing errored -- the caller simply
// could not be told apart from an unauthenticated one, which left the VS Code
// deploy panel unable to gate cut/deploy and showing a hedge to the one role
// that surface exists to serve.
//
// The fallback is still right for a value that genuinely is not a role
// (UNSPECIFIED, or a future proto value reaching an older client). What was
// wrong was reaching it for a role the wire had been carrying all along.
//
// The proto-vs-union half of the guard lives in Go, at
// scripts/ci/user_role_wire_parity_test.go, because it has to read
// memql.proto. This file pins the TS half: that the union maps, exhaustively,
// to sensible `Role` values.

import test from "node:test";
import assert from "node:assert/strict";

import { roleFromWire, accessSummaryFromWire, type Role } from "../src/client/types.js";
import type { UserRoleWire } from "../src/client/wire.js";

// Every member of UserRoleWire, spelled out. Typed as the union so a value
// added there without a line here fails the exhaustiveness check below rather
// than quietly going untested.
const EVERY_WIRE_ROLE: Record<UserRoleWire, Role> = {
  USER_ROLE_UNSPECIFIED: "",
  USER_ROLE_OWNER: "owner",
  USER_ROLE_ADMIN: "admin",
  USER_ROLE_DEVELOPER: "developer",
  USER_ROLE_WRITER: "writer",
  USER_ROLE_READER: "reader",
};

test("roleFromWire maps developer -- the memql#3331 regression", () => {
  assert.equal(roleFromWire("USER_ROLE_DEVELOPER"), "developer");
});

test("roleFromWire maps every wire role, and none falls through to \"\"", () => {
  for (const [wire, want] of Object.entries(EVERY_WIRE_ROLE) as [UserRoleWire, Role][]) {
    assert.equal(roleFromWire(wire), want, wire);
  }
  // UNSPECIFIED is the one legitimate "" -- it is the proto's zero value, not
  // a role. Asserted separately so the loop above cannot be read as
  // tolerating a silent "" for a real role.
  assert.equal(roleFromWire("USER_ROLE_UNSPECIFIED"), "");
  const named = Object.entries(EVERY_WIRE_ROLE).filter(([, role]) => role !== "");
  assert.equal(named.length, 5, "owner, admin, developer, writer, reader");
});

test("an absent or unknown wire role is indeterminate, not a role", () => {
  // undefined: the field was not set on the wire.
  assert.equal(roleFromWire(undefined), "");
  // A value this client does not know -- a newer server. Fails to "unknown",
  // which callers must treat as "could not resolve", never as least-privileged.
  assert.equal(roleFromWire("USER_ROLE_FROM_THE_FUTURE" as UserRoleWire), "");
});

test("accessSummaryFromWire carries developer through to clusterRole", () => {
  // The path the VS Code panel actually takes: MyAccessResult -> AccessSummary
  // -> roleVisibility. Mapping roleFromWire correctly is worth nothing if the
  // summary drops it.
  const summary = accessSummaryFromWire({
    requestId: "req-1",
    userId: "u-dev",
    primaryEmail: "dev@example.com",
    clusterRole: "USER_ROLE_DEVELOPER",
  });
  assert.notEqual(summary, null);
  assert.equal(summary?.clusterRole, "developer");
  assert.equal(summary?.userId, "u-dev");
});

test("accessSummaryFromWire is null for an absent payload", () => {
  assert.equal(accessSummaryFromWire(undefined), null);
});
