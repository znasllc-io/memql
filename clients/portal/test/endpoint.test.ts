// The bridge path derivation. Small, but it is the single point where the
// portal decides what to dial, and getting it wrong is a blank page with a
// failed WebSocket in the console rather than an error anyone can read.

import { describe, expect, it } from "vitest";

import { bridgePathFor } from "../src/cluster/endpoint";

describe("bridgePathFor", () => {
  it("derives the bridge path from the default mount", () => {
    expect(bridgePathFor("/portal/")).toBe("/memql/ws");
  });

  it("tolerates the mount without a trailing slash", () => {
    expect(bridgePathFor("/portal")).toBe("/memql/ws");
  });

  it("preserves a deployment base path in front of the mount", () => {
    // SERVER_PUBLIC_PATH=/memql registers /memql/portal/ and /memql/memql/ws
    // together, so the prefix has to survive.
    expect(bridgePathFor("/memql/portal/")).toBe("/memql/memql/ws");
  });

  it("stays relative -- it must resolve against the serving origin", () => {
    expect(bridgePathFor("/portal/").startsWith("/")).toBe(true);
  });
});
