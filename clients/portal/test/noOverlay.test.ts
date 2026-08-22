// A deploy console that cannot read an overlay says which situation it is in
// (memql#4265).
//
// Before this, the portal rendered the server's raw ENOENT and then, in the
// band below it, "Nothing is pinned in the overlay" -- which reads as "nothing
// is deployed" and is false. The Ship controls stayed clickable underneath.

import { describe, expect, it } from "vitest";

import { overlayAbsenceOf, overlayAbsenceStatement } from "../src/deploy/noOverlay";

describe("reading a deploy-console precondition", () => {
  it("tells the two absences apart by their machine-readable reason", () => {
    expect(
      overlayAbsenceOf(
        "FAILED_PRECONDITION: local_cluster: this is a local cluster, which is operated with `make up`",
      ),
    ).toBe("local_cluster");
    expect(
      overlayAbsenceOf(
        "FAILED_PRECONDITION: no_overlay_checkout: this node has no deploy checkout on disk",
      ),
    ).toBe("no_overlay_checkout");
  });

  it("treats a real failure as a failure, not a precondition", () => {
    // An unreadable-but-present overlay, a dead stream, a permission denial:
    // all still render as errors. Only the two named preconditions are
    // statements about how the installation runs.
    for (const other of [
      "",
      "INTERNAL: deploy console: parse overlay: yaml: line 3",
      "PERMISSION_DENIED: requires the owner or admin role",
      "UNAVAILABLE: connection closed",
    ]) {
      expect(overlayAbsenceOf(other)).toBe("");
    }
  });

  it("says something an operator can act on, in each case", () => {
    const local = overlayAbsenceStatement("local_cluster");
    expect(local).toContain("make up");
    expect(local).not.toContain("kustomization.yaml");

    const missing = overlayAbsenceStatement("no_overlay_checkout");
    expect(missing).toContain("MEMQL_DEPLOY_REPO_ROOT");

    // Neither pretends nothing is deployed.
    for (const statement of [local, missing]) {
      expect(statement).not.toContain("Nothing is pinned");
    }
  });
});
