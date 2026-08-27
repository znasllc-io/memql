// The OS module catalog for this PR is Profile only (memql#4706).
// Research is chrome, not a module. Phone allowlist is Profile
// (plus the research sheet, which is not listed here).

export const MODULE_IDS = ["profile"] as const;
export type ModuleId = (typeof MODULE_IDS)[number];

export const PHONE_ALLOWLIST: readonly ModuleId[] = ["profile"];

export function isModule(id: string): id is ModuleId {
  return (MODULE_IDS as readonly string[]).includes(id);
}

export function phoneAllows(id: string): boolean {
  return (PHONE_ALLOWLIST as readonly string[]).includes(id);
}
