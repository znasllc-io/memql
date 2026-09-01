import { describe, expect, it } from "vitest";

import { railFor } from "../../src/apps/deployables/packages/StageRail";
import type { DeploymentRow } from "../../src/apps/deployables/packages/rows";

// The stage rail is this surface's one new idea, and what it SAYS is the thing
// worth pinning -- so the assertions run against `railFor`, a pure function
// from a row to the stages to draw, rather than against a DOM.
//
// The case that matters most is the SKIPPED one. A rail that quietly omitted
// stage and roll would be a rail that never explained why an app-only deploy
// lands in seconds and restarts nothing, and an omission is exactly the kind
// of absence a test written against rendered output does not notice.

function deployment(over: Partial<DeploymentRow> = {}): DeploymentRow {
  return {
    id: "dep-1",
    packageId: "pkg-1",
    sourceVersion: "abc1234",
    status: "succeeded",
    report: { deployables: [], dslDomains: [], problems: [], ok: true },
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "",
    buildLogTail: "",
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "2026-09-01T12:00:30Z",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  };
}

function stage(row: DeploymentRow, id: string) {
  const found = railFor(row).stages.find((s) => s.id === id);
  if (found === undefined) throw new Error(`no stage ${id} on the rail`);
  return found;
}

describe("the stage rail", () => {
  it("marks stage and roll SKIPPED for a package with no MemQL, and says why", () => {
    const row = deployment({ report: { dslDomains: [], deployables: [], ok: true } });

    for (const id of ["staging_dsl", "rolling"]) {
      expect(stage(row, id).state).toBe("skipped");
    }
    // The reason is the whole value of drawing a skipped stage at all -- and
    // the two stages say DIFFERENT things, because stacking one sentence twice
    // reads as a stutter rather than as two facts.
    expect(stage(row, "staging_dsl").reason).toContain("ships no MemQL");
    expect(stage(row, "rolling").reason).toContain("nothing had to restart");
    expect(stage(row, "staging_dsl").reason).not.toBe(stage(row, "rolling").reason);
  });

  it("does NOT mark them skipped for a package that ships MemQL", () => {
    // The reachable positive for the case above: the same shape, one field
    // different, and the two stages are ordinary again. Without this, "skipped"
    // could be the only answer the function ever gives.
    const row = deployment({
      report: { dslDomains: [{ domain: "acme", constructs: { concept: 2 }, files: 1 }], deployables: [], ok: true },
      dslVersion: "packages/acme/deadbeef/",
    });

    for (const id of ["staging_dsl", "rolling"]) {
      expect(stage(row, id).state).toBe("done");
    }
  });

  it("says 'already the version running' when the MemQL was there but unchanged", () => {
    // Two different reasons for the same skip, and they are not
    // interchangeable: "this package has none" and "yours is already live"
    // send a reader to different conclusions about what just happened.
    const row = deployment({
      report: { dslDomains: [{ domain: "acme", constructs: {}, files: 1 }], deployables: [], ok: true },
      dslVersion: "",
      status: "succeeded",
    });
    expect(stage(row, "staging_dsl").reason).toContain("already the version this cluster is running");
  });

  it("marks the running stage and leaves the ones after it unreached", () => {
    const row = deployment({ status: "building", finishedAt: "" });
    expect(stage(row, "analyzing").state).toBe("done");
    expect(stage(row, "building").state).toBe("current");
    expect(stage(row, "publishing").state).toBe("ahead");
  });

  it("stops a failed run where it got to, and leaves every later stage unreached", () => {
    // This is the D6 guarantee made visible: a failure before publish means
    // nothing was published, and the rail is where somebody reads that.
    const row = deployment({
      status: "failed",
      buildLogTail: "npm ERR! missing script: build",
      deployables: [],
      report: { dslDomains: [], deployables: [], ok: true },
    });
    expect(stage(row, "analyzing").state).toBe("done");
    expect(stage(row, "building").state).toBe("stopped");
    expect(stage(row, "publishing").state).toBe("ahead");
  });

  it("a refusal during analysis stops at analysis", () => {
    const row = deployment({ status: "refused", report: null, buildLogTail: "", deployables: [] });
    expect(stage(row, "analyzing").state).toBe("stopped");
    expect(stage(row, "building").state).toBe("ahead");
  });

  it("keeps the D6 order, and keeps it in the DOM order even when read backwards", () => {
    // The rollback view renders the same stages reversed. The ORDER in the
    // returned array never changes -- only how it is read -- which is what
    // keeps a screen reader's order honest while the picture runs backwards.
    const ids = railFor(deployment()).stages.map((s) => s.id);
    expect(ids).toEqual(["analyzing", "awaiting_confirm", "building", "staging_dsl", "rolling", "publishing"]);
  });
});
