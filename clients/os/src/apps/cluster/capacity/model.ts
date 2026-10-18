export interface Budget { cpuMilli: number; memoryMiB: number; diskMiB: number; jobs: number }
export interface Capacity {
  available: boolean; reason?: string; observedAt: string;
  revision?: string; policyRevision?: string; ceiling?: Budget; budget?: Budget; reserved?: Budget;
  accepting: boolean; waiting?: number; running?: number;
}
const object = (value: unknown): Record<string, unknown> => {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The capacity report is incomplete.");
  return value as Record<string, unknown>;
};
const count = (value: unknown, minimum = 0): number => {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) throw new Error("The capacity report contains an invalid measurement.");
  return value;
};
function budget(value: unknown, minimum: number): Budget {
  const row = object(value);
  return { cpuMilli: count(row.cpuMilli, minimum), memoryMiB: count(row.memoryMiB, minimum), diskMiB: count(row.diskMiB, minimum), jobs: count(row.jobs, minimum) };
}
export function readCapacity(value: unknown): Capacity {
  const row = object(value);
  if (typeof row.available !== "boolean" || typeof row.observedAt !== "string" || !Number.isFinite(Date.parse(row.observedAt))) throw new Error("The capacity report is incomplete.");
  if (!row.available) return { available: false, accepting: false, observedAt: row.observedAt, reason: typeof row.reason === "string" ? row.reason : "Capacity is unavailable." };
  if (typeof row.revision !== "string" || !row.revision || typeof row.policyRevision !== "string" || !row.policyRevision || typeof row.accepting !== "boolean") throw new Error("The capacity settings are incomplete.");
  return { available: true, observedAt: row.observedAt, revision: row.revision, policyRevision: row.policyRevision,
    accepting: row.accepting, ceiling: budget(row.ceiling, 1), budget: budget(row.budget, 1),
    reserved: row.reserved == null ? undefined : budget(row.reserved, 0),
    waiting: row.waiting == null ? undefined : count(row.waiting), running: row.running == null ? undefined : count(row.running) };
}
export const cores = (milli: number) => (milli / 1000).toLocaleString(undefined, { maximumFractionDigits: 3 });
export const gib = (mib: number) => `${(mib / 1024).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB`;
