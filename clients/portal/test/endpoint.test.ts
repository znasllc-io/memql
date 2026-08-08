// The bridge path derivation. Small, but it is the single point where the
// portal decides what to dial, and getting it wrong is a blank page with a
// failed WebSocket in the console rather than an error anyone can read.

import { describe, expect, it } from "vitest";

import {
  bridgePathFor,
  clusterLabelFor,
  portalRedirectPathFor,
} from "../src/cluster/endpoint";

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

describe("portalRedirectPathFor", () => {
  // This value must equal, byte for byte, a redirect_uri registered for the
  // portal's OAuth client: component/identity/config.go matches by exact
  // string, so a trailing slash or a dropped prefix is a 400 at /authorize
  // ("Invalid redirect URI") rather than anything that hints at a path bug.
  it("derives the callback path from the mount", () => {
    expect(portalRedirectPathFor("/portal/")).toBe("/portal/auth/callback");
  });

  it("tolerates the mount without a trailing slash", () => {
    expect(portalRedirectPathFor("/portal")).toBe("/portal/auth/callback");
  });

  it("preserves a deployment base path", () => {
    expect(portalRedirectPathFor("/memql/portal/")).toBe("/memql/portal/auth/callback");
  });
});

describe("clusterLabelFor", () => {
  // Under the derive-from-origin registry decision the cluster IS the origin,
  // so its host is its name -- and the name the operator already recognises.
  it("names the cluster by the host that served the page", () => {
    expect(clusterLabelFor({ host: "cockpit.prod.example.com" })).toBe(
      "cockpit.prod.example.com",
    );
  });

  it("is empty rather than wrong when there is no location", () => {
    expect(clusterLabelFor(undefined)).toBe("");
  });
});
