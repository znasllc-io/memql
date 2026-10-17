import { describe, expect, it } from "vitest";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineFeatureStates, machineAppAvailability } from "../../src/apps/fleet/machines/features";
import { checksFor, EMPTY_DRAFT } from "../../src/apps/fleet/addMachine/flow";
import { installCommand } from "../../src/apps/fleet/addMachine/install";
import { machineRow } from "./harness";

const now = new Date("2026-10-17T03:00:00Z");
const ready = () => machineFromRow(machineRow({
  id: "feature-ready", connectedNodeId: "agent-b", lastSeenAt: now.toISOString(), capabilities: ["HEADLESS", "MODEL", "COMPUTERUSE", "WORKBENCH"],
  labels: { "model:qwen": "1", "model:qwen:kind": "chat" },
  capabilityDescriptor: { schemaVersion: 1, platform: "darwin", displayServer: "quartz", computerUseAvailable: true, actionContracts: { workerWorkbench: 1 } },
  permissions: { accessibility: false, screenRecording: false },
}));

describe("separate feature readiness", () => {
  it("keeps models, workbenches and headless commands available when desktop permissions are missing", () => {
    const states = Object.fromEntries(machineFeatureStates(ready(), now).map(state => [state.key, state.answer]));
    expect(states.models).toBe("Available"); expect(states.workbench).toBe("Available"); expect(states.headless).toBe("Available"); expect(states.computerUse).toBe("Needs setup");
    const checks = checksFor({ ...EMPTY_DRAFT, computerUse: true, workbench: true }, ready(), 2, now);
    expect(checks.find(c => c.id === "workbench")?.state).toBe("done");
    expect(checks.find(c => c.id === "permissions")?.state).not.toBe("done");
  });
  it("requires an implementation contract and reports private feature switches independently", () => {
    const machine = ready(); machine.workbenchContract = 0; machine.ownerFeatures = { headless: false, apps: false, computerUse: false, updatedAt: "" };
    const states = Object.fromEntries(machineFeatureStates(machine, now).map(state => [state.key, state.answer]));
    expect(states.workbench).toBe("Not set up"); expect(states.models).toBe("Available"); expect(states.apps).toBe("Disabled"); expect(states.headless).toBe("Disabled"); expect(states.computerUse).toBe("Disabled");
    expect(machineAppAvailability(machine, true, now)).toBe("Disabled in Settings");
  });
  it("includes workbench setup only when selected and refuses malformed runtime claims", () => {
    const command = (workbench: boolean) => installCommand({ platform: "mac", token: "fixture", clusterUrl: "https://api.example.test", computerUse: false, inference: false, workbench });
    expect(command(true)).toContain(" --workbench"); expect(command(false)).not.toContain(" --workbench");
    const machine = machineFromRow(machineRow({ id: "malformed", capabilityDescriptor: { actionContracts: { workerWorkbench: "1" } } }));
    expect(machine.workbenchContract).toBe(0);
  });
});
