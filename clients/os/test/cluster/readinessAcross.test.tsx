import { describe, expect, it } from "vitest";

import { readinessNodeLines, unknownReasonWords } from "../../src/apps/cluster/modules/rows";
import { foldReadiness, type LaneReport, type NodeLiveness, type NodeReport } from "../../src/system/readinessFold";

// A MODULE ACROSS THE CLUSTER, as the Cluster app's Modules detail lists it
// (memql#5259): every live node's own answer and how fresh it is, voters
// first, then -- drawn quieter -- the nodes the fold set aside, each with the
// time it last checked or the reason it could not.

const NOW = new Date("2026-09-09T15:43:02Z");

function lanes(local: boolean): LaneReport[] {
  const live = (present: boolean) => [{ name: "live", present, source: "" }];
  return [
    { name: "local", configurableFrom: "fleet", scope: "cluster", complete: local, slots: live(local) },
    { name: "app", configurableFrom: "fleet", scope: "cluster", complete: false, slots: live(false) },
    { name: "federation", configurableFrom: "deployment", complete: false, slots: live(false) },
  ];
}

const REPORTS: NodeReport[] = [
  { module: "ai", nodeId: "agent-a", nodeType: "agent", state: "configured", core: true, lanes: lanes(true), reportedAt: "2026-09-09T15:42:54Z" },
  { module: "ai", nodeId: "edge-a", nodeType: "edge", state: "unconfigured", core: true, lanes: lanes(false), reportedAt: "2026-09-09T09:05:02Z" },
  { module: "ai", nodeId: "bff-b", nodeType: "bff", state: "unknown", reason: "fleetReadFailed", core: true, lanes: [], reportedAt: "2026-09-09T15:40:00Z" },
];

const LIVE: NodeLiveness[] = ["agent-a", "edge-a", "bff-b"].map((nodeId) => ({
  nodeId,
  health: "healthy",
  lastSeen: "2026-09-09T15:42:50Z",
}));

describe("readinessNodeLines", () => {
  it("lists the voters, then the node catching up, then the node that could not check", () => {
    const [ai] = foldReadiness(REPORTS, LIVE, NOW);
    expect(ai?.state).toBe("configured");
    expect(readinessNodeLines(ai!, NOW)).toEqual([
      {
        nodeId: "agent-a",
        nodeType: "agent",
        words: "Set up",
        note: "checked 8s ago",
        counted: true,
        at: "2026-09-09T15:42:54Z",
      },
      {
        nodeId: "edge-a",
        nodeType: "edge",
        words: "Catching up",
        note: "last checked 6h ago, before the last change; it re-checks on its own",
        counted: false,
        at: "2026-09-09T09:05:02Z",
      },
      {
        nodeId: "bff-b",
        nodeType: "bff",
        words: "Could not check",
        note: "the fleet could not be read; it retries on its own",
        counted: false,
        at: "2026-09-09T15:40:00Z",
      },
    ]);
  });

  // The relative words and the exact moment answer different questions, and
  // the list needs both: "checked 2m ago" on two nodes is an order a reader
  // cannot see, and that order is what the fold's staleness rule turns on.
  // A node that reported no time at all carries "" rather than a guess, which
  // is what keeps the title off the row instead of showing an invented one.
  it("carries each node's exact reported moment beside the relative words", () => {
    const [ai] = foldReadiness(REPORTS, LIVE, NOW);
    const lines = readinessNodeLines(ai!, NOW);
    expect(lines.map((l) => l.at)).toEqual([
      "2026-09-09T15:42:54Z",
      "2026-09-09T09:05:02Z",
      "2026-09-09T15:40:00Z",
    ]);

    const timeless = foldReadiness(
      [{ module: "ai", nodeId: "agent-z", nodeType: "agent", state: "configured", core: true, lanes: lanes(true), reportedAt: "" }],
      [{ nodeId: "agent-z", health: "healthy", lastSeen: "2026-09-09T15:42:50Z" }],
      NOW,
    );
    expect(readinessNodeLines(timeless[0]!, NOW)[0]?.at).toBe("");
  });

  // The reason on a row is a CLOSED vocabulary and never an error string, so
  // an unrecognised one says only that the check did not finish.
  it("words every reason the engine writes, and nothing it does not", () => {
    expect(unknownReasonWords("fleetReadFailed")).toBe("the fleet could not be read");
    expect(unknownReasonWords("integrationProbeFailed")).toBe("the integration's status check failed");
    expect(unknownReasonWords("somethingNew")).toBe("the check did not finish");
    expect(unknownReasonWords(undefined)).toBe("the check did not finish");
  });
});
