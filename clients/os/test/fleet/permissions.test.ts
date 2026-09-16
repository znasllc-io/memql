import { describe, expect, it } from "vitest";
import { permissionsFrom, permissionDecision, machineFromRow } from "../../src/apps/fleet/rows";
import { checksFor, EMPTY_DRAFT } from "../../src/apps/fleet/addMachine/flow";
import { machineRow } from "./harness";

describe("measured worker permissions", () => {
  it("does not turn placeholders or absent fields into denials", () => {
    const stub = permissionsFrom({ accessibility: false, screen_recording: false, detail: "permission probe not yet implemented (MVP)" });
    expect(permissionDecision(stub, "accessibility")).toBe("unknown");
    expect(permissionDecision(stub, "screenRecording")).toBe("unknown");
    const partial = permissionsFrom({ accessibility: true });
    expect(permissionDecision(partial, "accessibility")).toBe("granted");
    expect(permissionDecision(partial, "screenRecording")).toBe("unknown");
  });
  it.each(["unknown", "unexpected"])("uses explicit %s over a stale legacy grant", state => {
    const p = permissionsFrom({ accessibility: true, accessibility_state: state, checked_at: "2026-09-15T18:00:00Z", probe_context: "worker-process" });
    expect(permissionDecision(p, "accessibility")).toBe("unknown");
    expect(p.accessibility).toBe(false);
    expect(p.checkedAt).toBe("2026-09-15T18:00:00Z");
    expect(p.probeContext).toBe("worker-process");
  });
  it("checks off individual grants while withholding overall completion", () => {
    const machine = machineFromRow(machineRow({ id: "v1:worker:registration:permission-test", permissions: { accessibility_state: "granted", screen_recording_state: "unknown" } }));
    const check = checksFor({ ...EMPTY_DRAFT, platform: "mac", computerUse: true }, machine, 0, new Date()).find(c => c.id === "permissions")!;
    expect(check.state).toBe("unknown");
    expect(check.details).toMatchObject([{ name: "Accessibility", state: "done" }, { name: "Screen Recording", state: "unknown" }]);
    expect(check.command).toBeUndefined();
  });
});
