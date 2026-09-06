import { describe, expect, it } from "vitest";

import {
  chipsFromMap,
  labelMapFrom,
  mapFromChips,
  mergeLabels,
  parseLabelChip,
} from "../../src/apps/fleet/labels";
import { formatDuration, formatFreshness, formatMoment } from "../../src/kit/format";
import { figureValue } from "../../src/cluster/figure";
import {
  activePolicy,
  appSessionDetailFromRow,
  appSessionFromRow,
  delegationPolicyFromRow,
  invocationFromRow,
  isRevoked,
  latestPerId,
  machineFromRow,
  machineName,
  nodeFromRow,
  routingPolicyFromRow,
  sessionIsLive,
  sessionsNewestFirst,
  workspaceFromRow,
  workspacesByNode,
} from "../../src/apps/fleet/rows";

describe("label maps", () => {
  it("keeps strings, coerces numbers and booleans, drops the rest", () => {
    expect(labelMapFrom({ os: "darwin", cores: 8, gpu: true, nested: { a: 1 } })).toEqual({
      os: "darwin",
      cores: "8",
      gpu: "true",
    });
  });

  it("is empty for a non-object, an array, and null", () => {
    expect(labelMapFrom(null)).toEqual({});
    expect(labelMapFrom(["os=darwin"])).toEqual({});
    expect(labelMapFrom("os=darwin")).toEqual({});
  });

  it("merges with the operator side winning and reports overrides", () => {
    const merged = mergeLabels({ os: "darwin", tier: "cheap" }, { tier: "gold", room: "studio" });
    expect(merged).toEqual([
      { key: "os", value: "darwin", source: "reported", overrides: false },
      // Operator-only: it wins, but it OVERRIDES nothing.
      { key: "room", value: "studio", source: "operator", overrides: false },
      // The case that matters -- the machine says one thing, routing acts on
      // another.
      { key: "tier", value: "gold", source: "operator", overrides: true },
    ]);
  });

  it("splits a chip on the FIRST separator only", () => {
    expect(parseLabelChip("path=/opt/a=b")).toEqual({ key: "path", value: "/opt/a=b" });
  });

  it("refuses a chip with no key and a bare word", () => {
    expect(parseLabelChip("=orphan")).toBeNull();
    expect(parseLabelChip("gpu")).toBeNull();
  });

  it("round-trips a map through chips, dropping unparseable ones", () => {
    const map = { "has-blender": "true", os: "linux" };
    expect(chipsFromMap(map)).toEqual(["has-blender=true", "os=linux"]);
    expect(mapFromChips(["has-blender=true", "os=linux", "junk"])).toEqual(map);
  });
});

describe("machineFromRow", () => {
  const row = {
    id: "v1:worker:registration:abc",
    ownerUserId: "u1",
    name: "studio.local",
    displayName: "Studio mini",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "studio.local" },
    capabilityDescriptor: { displayServer: "quartz", computerUseAvailable: true },
    labels: { os: "darwin", tier: "cheap" },
    operatorLabels: { tier: "gold" },
    concurrency: { HEADLESS: 8, COMPUTERUSE: "1" },
    activeCount: 2,
    lastSeenAt: "2026-08-30T10:00:00Z",
    apps: [
      { id: "claude-code", version: "1.2.3", allowed: true, signedIn: true, subscription: "present" },
      { id: "codex", version: "0.9", allowed: true, signedIn: false, subscription: "unknown" },
      { id: "some-other-app", allowed: true, signedIn: true },
      "not an object",
      { version: "1.0" },
    ],
  };

  it("projects the platform facts and the two label maps", () => {
    const m = machineFromRow(row);
    expect(m.os).toBe("darwin");
    expect(m.arch).toBe("arm64");
    expect(m.platform).toBe("darwin/arm64");
    expect(m.hostname).toBe("studio.local");
    expect(m.displayServer).toBe("quartz");
    expect(m.computerUseAvailable).toBe(true);
    expect(m.reportedLabels).toEqual({ os: "darwin", tier: "cheap" });
    expect(m.operatorLabels).toEqual({ tier: "gold" });
    expect(m.concurrency).toEqual({ HEADLESS: 8, COMPUTERUSE: 1 });
    expect(m.activeCount).toBe(2);
  });

  it("names a machine by displayName, then the reported name, then the id", () => {
    expect(machineName(machineFromRow(row))).toBe("Studio mini");
    expect(machineName(machineFromRow({ ...row, displayName: "  " }))).toBe("studio.local");
    expect(machineName(machineFromRow({ ...row, displayName: "", name: "" }))).toBe(
      "v1:worker:registration:abc",
    );
  });

  it("marks an app runnable only when the engine drives it, it is allowed, and it is signed in", () => {
    const apps = machineFromRow(row).apps;
    // Sorted by id, and the two malformed entries are dropped rather than
    // rendered as a nameless app.
    expect(apps.map((a) => a.id)).toEqual(["claude-code", "codex", "some-other-app"]);
    expect(apps[0]).toMatchObject({ label: "Claude Code", runnable: true, why: "" });
    expect(apps[1]).toMatchObject({ runnable: false, why: "not signed in" });
    // An id outside the engine's CLOSED set is DISPLAYED -- the machine
    // really has it -- and never marked runnable.
    expect(apps[2]).toMatchObject({
      label: "some-other-app",
      runnable: false,
      why: "this engine does not drive it",
    });
  });

  it("reads a payload-nested row identically to a flat one", () => {
    const nested = { id: "v1:worker:registration:abc", payload: { ...row, id: "inner" } };
    expect(machineFromRow(nested).displayName).toBe("Studio mini");
    // The envelope's id wins: it is the row's own id, and the nested copy is
    // whatever the payload happened to carry.
    expect(machineFromRow(nested).id).toBe("v1:worker:registration:abc");
  });

  it("treats a written revokedAt as revoked and a blank one as live", () => {
    expect(isRevoked(machineFromRow({ ...row, revokedAt: "2026-08-30T11:00:00Z" }))).toBe(true);
    expect(isRevoked(machineFromRow(row))).toBe(false);
  });
});

describe("routing policy rows", () => {
  it("takes the newest ACTIVE row and ignores superseded ones", () => {
    const rows = [
      routingPolicyFromRow({ id: "p2", strategy: "leastLoaded", fallback: "none", active: false }),
      routingPolicyFromRow({ id: "p1", strategy: "labelMatch", fallback: "nextMatching", active: true }),
    ];
    expect(activePolicy(rows)?.id).toBe("p1");
  });

  it("answers null when the caller has no active row at all", () => {
    expect(activePolicy([])).toBeNull();
    expect(
      activePolicy([routingPolicyFromRow({ id: "p2", active: false })]),
    ).toBeNull();
  });
});

describe("invocation routing records", () => {
  it("reads a full record, preserving candidate ORDER", () => {
    const call = invocationFromRow({
      id: "i1",
      tool: "workerHost",
      action: "exec",
      outcome: "rerouted",
      durationMs: 1500,
      routing: {
        policyId: "p1",
        strategy: "labelMatch",
        candidatesConsidered: ["reg-b", "reg-a", "reg-c"],
        attempts: 2,
        selectedBy: "policy",
        reroutedFrom: "workbench",
        requireLabels: { gpu: "true" },
        preferLabels: { room: "studio" },
      },
    });
    expect(call.routing.present).toBe(true);
    expect(call.routing.candidatesConsidered).toEqual(["reg-b", "reg-a", "reg-c"]);
    expect(call.routing.attempts).toBe(2);
    expect(call.routing.reroutedFrom).toBe("workbench");
    expect(call.routing.requireLabels).toEqual({ gpu: "true" });
  });

  it("reports an EMPTY routing object as absent, not as a decision with no candidates", () => {
    const call = invocationFromRow({ id: "i2", outcome: "denied_by_scope", routing: {} });
    expect(call.routing.present).toBe(false);
    expect(call.routing.candidatesConsidered).toEqual([]);
  });

  it("reports a MISSING routing field as absent too", () => {
    expect(invocationFromRow({ id: "i3", outcome: "success" }).routing.present).toBe(false);
  });
});

describe("workspaces and replicas", () => {
  it("groups workspaces by replica, newest first inside each", () => {
    const rows = [
      workspaceFromRow({ id: "w1", nodeId: "wb-1", createdAt: "2026-08-01T00:00:00Z" }),
      workspaceFromRow({ id: "w2", nodeId: "wb-2", createdAt: "2026-08-03T00:00:00Z" }),
      workspaceFromRow({ id: "w3", nodeId: "wb-1", createdAt: "2026-08-02T00:00:00Z" }),
    ];
    expect(workspacesByNode(rows)).toEqual([
      { nodeId: "wb-1", workspaces: [expect.objectContaining({ id: "w3" }), expect.objectContaining({ id: "w1" })] },
      { nodeId: "wb-2", workspaces: [expect.objectContaining({ id: "w2" })] },
    ]);
  });

  it("keeps a workspace whose replica was never stamped, in its own group", () => {
    const groups = workspacesByNode([workspaceFromRow({ id: "w9", nodeId: "" })]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.nodeId).toBe("");
  });

  it("collapses the append-only node history to the latest row per id", () => {
    const nodes = [
      nodeFromRow({ id: "wb-1", nodeType: "workbench", health: "starting", createdAt: "2026-08-01T00:00:00Z" }),
      nodeFromRow({ id: "wb-1", nodeType: "workbench", health: "healthy", createdAt: "2026-08-02T00:00:00Z" }),
      nodeFromRow({ id: "wb-2", nodeType: "workbench", health: "stopped", createdAt: "2026-08-01T00:00:00Z" }),
    ];
    const latest = latestPerId(nodes);
    expect(latest.map((n) => `${n.id}:${n.health}`)).toEqual(["wb-1:healthy", "wb-2:stopped"]);
  });
});

describe("the fleet's time voice", () => {
  const now = new Date("2026-08-30T12:00:00Z");

  it("says never for a machine that has not checked in, which is not the same as unreadable", () => {
    expect(formatFreshness("", now)).toBe("never");
    expect(formatFreshness("not a date", now)).toBe("not a date");
  });

  it("reads clock skew as just now rather than as a negative duration", () => {
    expect(formatFreshness("2026-08-30T12:00:30Z", now)).toBe("just now");
  });

  it("coarsens as the gap grows", () => {
    expect(formatFreshness("2026-08-30T11:59:15Z", now)).toBe("45s ago");
    expect(formatFreshness("2026-08-30T11:30:00Z", now)).toBe("30m ago");
    expect(formatFreshness("2026-08-29T12:00:00Z", now)).toBe("24h ago");
    expect(formatFreshness("2026-08-20T12:00:00Z", now)).toBe("10d ago");
  });

  it("renders an unmeasured duration as -- rather than as zero", () => {
    expect(formatDuration(0)).toBe("--");
    expect(formatDuration(-1)).toBe("--");
    expect(formatDuration(1500)).toBe("1.5s");
    expect(formatDuration(430)).toBe("430ms");
  });

  it("renders a blank moment as -- and an unparseable one verbatim", () => {
    expect(formatMoment("")).toBe("--");
    expect(formatMoment("whenever")).toBe("whenever");
  });
});

// The Apps section's projections (epic memql#5009). PURE, so the one rule
// that matters most -- an unreported usage is ABSENT, never zero -- is
// asserted on a function of a row rather than through three render layers.
describe("appSessionFromRow", () => {
  it("reads an ABSENT usage object as unmeasured, never as three zeros", () => {
    const session = appSessionFromRow({
      id: "v1:worker:appSession:a",
      app: "claude-code",
      status: "ended",
      billing: "subscription",
    });
    expect(session.usage.known).toBe(false);
    // An app that reported nothing did not report zero. These lead to
    // opposite conclusions about a run, so they are different VALUES.
    expect(session.usage.inputTokens.kind).toBe("absent");
    expect(session.usage.outputTokens.kind).toBe("absent");
    expect(session.usage.costUSD.kind).toBe("absent");
    expect(figureValue(session.usage.inputTokens)).toBeNull();
  });

  it("reads a REPORTED zero as a measurement, because it is one", () => {
    const session = appSessionFromRow({
      id: "v1:worker:appSession:a",
      usage: { known: true, inputTokens: 0, outputTokens: 0, costUSD: 0 },
    });
    expect(session.usage.known).toBe(true);
    expect(figureValue(session.usage.inputTokens)).toBe(0);
  });

  it("refuses a usage the app did not vouch for, even when numbers are present", () => {
    // `known: false` is the app's own answer to "did you say anything". A
    // partial object that arrived without it is not a report.
    const session = appSessionFromRow({
      id: "v1:worker:appSession:a",
      usage: { inputTokens: 900 },
    });
    expect(session.usage.known).toBe(false);
    expect(session.usage.inputTokens.kind).toBe("absent");
  });

  it("falls to `unknown` billing rather than to a blank chip", () => {
    expect(appSessionFromRow({ id: "v1:worker:appSession:a" }).billing).toBe("unknown");
  });

  it("keeps the transcript byte for byte and reads truncation as a flag", () => {
    const detail = appSessionDetailFromRow({
      id: "v1:worker:appSession:a",
      transcript: "  keep   this\n\n",
      transcriptBytes: 15,
      transcriptTruncated: true,
    });
    expect(detail.transcript).toBe("  keep   this\n\n");
    expect(figureValue(detail.transcriptBytes)).toBe(15);
    expect(detail.transcriptTruncated).toBe(true);
  });

  it("reads an ABSENT transcriptBytes as unmeasured rather than as 0 bytes", () => {
    const detail = appSessionDetailFromRow({ id: "v1:worker:appSession:a" });
    expect(detail.transcriptBytes.kind).toBe("absent");
  });

  it("names starting and running as the only live statuses", () => {
    expect(sessionIsLive("starting")).toBe(true);
    expect(sessionIsLive("running")).toBe(true);
    for (const done of ["ended", "failed", "cancelled", ""]) {
      expect(sessionIsLive(done)).toBe(false);
    }
  });

  it("orders newest first, breaking a tie by id so arrival order never decides", () => {
    const rows = [
      { id: "b", startedAt: "2026-09-01T09:00:00Z" },
      { id: "a", startedAt: "2026-09-02T09:00:00Z" },
      { id: "c", startedAt: "2026-09-01T09:00:00Z" },
    ].map((r) => appSessionFromRow(r));
    expect(sessionsNewestFirst(rows).map((r) => r.id)).toEqual(["a", "c", "b"]);
  });
});

describe("delegationPolicyFromRow", () => {
  it("reads a ZERO maxConcurrentSessions as the default, never as none", () => {
    // The concept says so, and it has to: a zero here would silently disable
    // a feature the person turned on.
    const policy = delegationPolicyFromRow({
      id: "p",
      ownerUserId: "u",
      preferSubscriptionApps: true,
      maxConcurrentSessions: 0,
      credentialLifetimeSeconds: 0,
    });
    expect(policy.maxConcurrentSessions).toBe(1);
    expect(policy.credentialLifetimeSeconds).toBe(14400);
  });

  it("keeps appOrder in the order the row stored it -- the list IS the priority", () => {
    const policy = delegationPolicyFromRow({
      id: "p",
      appOrder: ["codex", "claude-code"],
      eligibleKinds: ["runCommand", 7, "callTool"],
    });
    expect(policy.appOrder).toEqual(["codex", "claude-code"]);
    // A non-string member is dropped rather than coerced: it is not a kind.
    expect(policy.eligibleKinds).toEqual(["runCommand", "callTool"]);
  });

  it("reads an absent master switch as OFF", () => {
    expect(delegationPolicyFromRow({ id: "p" }).preferSubscriptionApps).toBe(false);
  });
});
