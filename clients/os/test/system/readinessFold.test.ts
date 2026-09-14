import { readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import {
  foldReadiness,
  nodeIsLive,
  NODE_LIVE_WINDOW_SECONDS,
  type NodeLiveness,
  type NodeReport,
} from "../../src/system/readinessFold";

// THE SAME FIXTURES THE ENGINE RUNS. component/memql/readiness/fold_test.go reads
// this directory too; a case one side passes and the other fails is the
// drift this test exists to catch. The directory is resolved from this file,
// so it does not depend on the working directory vitest was started from.
const FIXTURES = resolve(__dirname, "../../../../component/memql/readiness/testdata/fold");

interface Fixture {
  name: string;
  now: string;
  reports: NodeReport[];
  nodes: NodeLiveness[];
  expect: { module: string; state: string; disagreement: string[]; unknown?: string[]; stale?: string[] }[];
}

describe("the fold mirrors component/memql/readiness", () => {
  const files = readdirSync(FIXTURES)
    .filter((f) => f.endsWith(".json"))
    .sort();

  // Without this, a mistyped path reads as a suite with no cases and passes.
  it("finds the shared fixtures", () => {
    expect(files.length).toBeGreaterThanOrEqual(15);
  });

  for (const file of files) {
    const fx = JSON.parse(readFileSync(resolve(FIXTURES, file), "utf8")) as Fixture;
    it(fx.name, () => {
      const got = foldReadiness(fx.reports, fx.nodes, new Date(fx.now));
      expect(
        got.map((v) => ({
          module: v.module,
          state: v.state,
          disagreement: v.disagreement,
          unknown: v.unknown,
          stale: v.stale,
        })),
      ).toEqual(fx.expect.map((e) => ({ ...e, unknown: e.unknown ?? [], stale: e.stale ?? [] })));
    });
  }

  // THE LANES ARE THE SHELL'S HALF ALONE, so this half of the parity is
  // asserted here and nowhere else. The Go Verdict deliberately carries no
  // lanes -- its consumer, the fold builtin, does not read them -- while the
  // shell's Set up group and the core gate need the slot names to say what is
  // still missing. So the shared fixture holds the two sides equal on module,
  // state and disagreement, and this holds the shell to carrying the lanes
  // through at all.
  //
  // Without it, `lanes: rs[0].lanes ?? []` could quietly become `[]` and every
  // surface that reads a lane would render an empty list -- which looks
  // exactly like a module with nothing configured.
  it("carries the worst live reporter's inference lanes through the fold", () => {
    const fx = JSON.parse(
      readFileSync(resolve(FIXTURES, "08-inference-lanes.json"), "utf8"),
    ) as Fixture;
    const [ai] = foldReadiness(fx.reports, fx.nodes, new Date(fx.now));
    expect(ai?.lanes.map((l) => l.name)).toEqual(["local", "app", "federation"]);
    // Configured, and NOT live: the machine is asleep. Two facts on one lane,
    // which is the whole of the design record's D3.
    expect(ai?.lanes.find((l) => l.name === "local")?.complete).toBe(true);
    expect(ai?.lanes.find((l) => l.name === "local")?.slots).toEqual([
      { name: "live", present: false },
    ]);
  });

  // THE SET-ASIDE ROWS ARE THE SHELL'S HALF TOO. The Go Verdict names the
  // stale and unknown nodes and no more; the Cluster app's per-node reading
  // needs each one's own time and reason to say "behind since 09:14" or "the
  // fleet read failed". So the shared fixture holds the ids equal and this
  // holds the shell to carrying the rows through.
  it("carries each set-aside node's own row, stale and unknown alike", () => {
    const fx = JSON.parse(
      readFileSync(resolve(FIXTURES, "15-stale-unknown-and-dead-together.json"), "utf8"),
    ) as Fixture;
    const [ai] = foldReadiness(fx.reports, fx.nodes, new Date(fx.now));
    expect(ai?.aside).toEqual([
      {
        nodeId: "edge-a",
        nodeType: "edge",
        state: "unconfigured",
        reportedAt: "2026-09-13T09:00:00Z",
        why: "stale",
      },
      {
        nodeId: "bff-b",
        nodeType: "bff",
        state: "unknown",
        reportedAt: "2026-09-13T12:00:20Z",
        reason: "fleetReadFailed",
        why: "unknown",
      },
    ]);
    // Set aside means NOT a voter: neither list may be read as a vote.
    expect(ai?.nodes.map((n) => n.nodeId)).toEqual(["agent-a", "bff-a"]);
  });

  it("nodeIsLive needs a live health word and a recent heartbeat", () => {
    const now = new Date("2026-09-06T12:00:00Z");
    const fresh = new Date(now.getTime() - (NODE_LIVE_WINDOW_SECONDS / 2) * 1000).toISOString();
    const stale = new Date(now.getTime() - (NODE_LIVE_WINDOW_SECONDS + 1) * 1000).toISOString();
    expect(nodeIsLive({ nodeId: "a", health: "healthy", lastSeen: fresh }, now)).toBe(true);
    expect(nodeIsLive({ nodeId: "a", health: "stopped", lastSeen: fresh }, now)).toBe(false);
    expect(nodeIsLive({ nodeId: "a", health: "healthy", lastSeen: stale }, now)).toBe(false);
    expect(nodeIsLive({ nodeId: "a", health: "healthy", lastSeen: "" }, now)).toBe(false);
    expect(nodeIsLive({ nodeId: "a", health: "healthy", lastSeen: "not a date" }, now)).toBe(false);
  });

  // Every live health word counts, and the set is the one the Go side holds.
  // A node mid-rollout is `draining` and still reporting; dropping it would
  // make a rolling restart look like a cluster that forgot its configuration.
  it("counts every live health word", () => {
    const now = new Date("2026-09-06T12:00:00Z");
    const seen = new Date(now.getTime() - 1000).toISOString();
    for (const health of ["healthy", "connecting", "degraded", "draining"]) {
      expect(nodeIsLive({ nodeId: "a", health, lastSeen: seen }, now)).toBe(true);
    }
    for (const health of ["stopped", "", "HEALTHY"]) {
      expect(nodeIsLive({ nodeId: "a", health, lastSeen: seen }, now)).toBe(false);
    }
  });
});
