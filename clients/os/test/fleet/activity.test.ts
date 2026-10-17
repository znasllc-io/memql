import { expect, it } from "vitest";
import { machineActivity } from "../../src/apps/fleet/machines/activity";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineRow } from "./harness";

const now = new Date("2026-10-17T02:00:00Z");
function machine(report: Record<string, unknown> | null) {
  return machineFromRow(machineRow({ id: "a", lastSeenAt: now.toISOString(), connectedNodeId: "agent-a", sharingActivity: report }));
}
const report = { nodeId: "agent-a", observedAt: now.toISOString(), ownerCalls: 2, sharedCalls: 3, users: [{ id: "ana", activeCalls: 3 }], groups: [{ id: "design", activeCalls: 3 }] };

it("counts unique calls from one snapshot instead of overlapping grants or a different heartbeat", () => {
  const m = machine(report); m.activeCount = 99;
  expect(machineActivity(m, now)).toEqual({ owner: 2, shared: 3, total: 5 });
});

it.each([
  null,
  { ...report, ownerCalls: undefined },
  { ...report, sharedCalls: -1 },
  { ...report, sharedCalls: "3" },
  { ...report, sharedCalls: 1.5 },
  { ...report, nodeId: "former-replica" },
  { ...report, observedAt: "invalid" },
  { ...report, observedAt: new Date(now.getTime() - 46_000).toISOString() },
  { ...report, observedAt: new Date(now.getTime() + 60_000).toISOString() },
])("keeps an unavailable or stale breakdown unknown", data => {
  expect(machineActivity(machine(data), now)).toBeNull();
});

it("keeps a measured zero distinct from offline or unreported activity", () => {
  const m = machine({ ...report, ownerCalls: 0, sharedCalls: 0 });
  expect(machineActivity(m, now)).toEqual({ owner: 0, shared: 0, total: 0 });
  m.connectedNodeId = "";
  expect(machineActivity(m, now)).toBeNull();
});
