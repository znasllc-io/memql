import { describe, expect, it } from "vitest";
import { deployPackage } from "../../src/apps/deployables/packages/calls";
import { fakeConnection } from "./harness";

describe("saved deployment confirmation", () => {
  it("returns the accepted run without waiting for its build and keeps the snapshot and scope", async () => {
    const connection = fakeConnection({ deployResult: { deploymentId: "saved-run", status: "analyzing", awaitingConfirm: false } });
    const result = await deployPackage(connection.query, "source", {
      confirm: true,
      deploymentId: "saved-run",
      placements: { sibling: { skip: true, hostname: "", accountId: "", ownDomain: "" } },
    });
    const calls = connection.callsNamed("packageDeploy");
    expect(calls).toHaveLength(1);
    expect(calls[0]).toContain("background: true");
    expect(calls[0]).toContain("confirm: true");
    expect(calls[0]).toContain('deploymentId: "saved-run"');
    expect(calls[0]).toContain("sibling: {skip: true}");
    expect(calls[0]).not.toContain("fromDeploymentId:");
    expect(result).toEqual({ deploymentId: "saved-run", status: "analyzing", awaitingConfirm: false });
  });

  it("keeps fresh synchronous confirmation and background analysis contracts", async () => {
    const connection = fakeConnection({});
    await deployPackage(connection.query, "source", { confirm: true });
    await deployPackage(connection.query, "source", { confirm: false });
    const calls = connection.callsNamed("packageDeploy");
    expect(calls).toHaveLength(2);
    expect(calls[0]).not.toContain("background:");
    expect(calls[1]).toContain("background: true");
    expect(calls[1]).toContain("confirm: false");
  });
});
