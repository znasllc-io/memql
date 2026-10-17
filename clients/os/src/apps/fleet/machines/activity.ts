import { isWorkerOnline } from "../online";
import type { MachineRow } from "../rows";

export interface MachineActivity { owner: number; shared: number; total: number }

/** One snapshot from the current stream holder. Grant rows can overlap and
 * must never be summed to infer the number of borrowed calls. */
export function machineActivity(machine: MachineRow, now: Date): MachineActivity | null {
  const report = machine.sharingActivity;
  if (!isWorkerOnline(machine, now) || !report || report["nodeId"] !== machine.connectedNodeId) return null;
  const age = now.getTime() - Date.parse(String(report["observedAt"] ?? ""));
  if (!Number.isFinite(age) || age < -5_000 || age > 45_000) return null;
  const owner = report["ownerCalls"], shared = report["sharedCalls"];
  if (typeof owner !== "number" || !Number.isSafeInteger(owner) || owner < 0 ||
      typeof shared !== "number" || !Number.isSafeInteger(shared) || shared < 0 ||
      !Number.isSafeInteger(owner + shared)) return null;
  return { owner, shared, total: owner + shared };
}
