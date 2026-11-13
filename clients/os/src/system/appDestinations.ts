/** A moved surface retains the SAME app + section capability conjunction.
 * These are durable grants, not display names. Moving a page must neither
 * revoke a custom grant nor admit the destination app's unrelated sections. */
export const SECTION_RELOCATIONS = [
  { fromApp: "settings", fromSection: "cluster", toApp: "identity", toSection: "policy", resource: "app:settings/cluster" },
  { fromApp: "settings", fromSection: "keys", toApp: "identity", toSection: "keys", resource: "app:settings/keys" },
  { fromApp: "settings", fromSection: "tokens", toApp: "identity", toSection: "service-tokens", resource: "app:settings/tokens" },
  { fromApp: "users", fromSection: "requests", toApp: "identity", toSection: "requests", resource: "app:users" },
  { fromApp: "cluster", fromSection: "audit", toApp: "identity", toSection: "audit", resource: "app:cluster/audit" },
  { fromApp: "settings", fromSection: "benchmarks", toApp: "cluster", toSection: "benchmarks", resource: "app:settings/benchmarks" },
  { fromApp: "settings", fromSection: "language", toApp: "concepts", toSection: "language", resource: "app:settings/language" },
  { fromApp: "settings", fromSection: "access", toApp: "users", toSection: "access", resource: "app:settings/access" },
  { fromApp: "cluster", fromSection: "origins", toApp: "settings", toSection: "data", resource: "app:cluster/origins" },
] as const;

/** The read-only half of old Settings > Cluster also lives in Overview. */
export function relocatedSectionSource(appId: string, sectionId: string, resource?: string): string | undefined {
  if (appId === "cluster" && sectionId === "facts" && resource === "app:settings/cluster") return "settings";
  return SECTION_RELOCATIONS.find(move => move.toApp === appId && move.toSection === sectionId && move.resource === resource)?.fromApp;
}

/** Personal surfaces retain only their original app door when an older custom
 * role has no Profile grant yet. A Profile decision, including a deny, wins. */
export function legacyPersonalSource(sectionId: string): string | undefined {
  if (["profile", "devices", "tokens", "settings"].includes(sectionId)) return "identity";
  if (["about", "appearance", "ask", "apps", "diagnostics"].includes(sectionId)) return "settings";
  return undefined;
}

/** Old saved links keep their meaning after the approved personal-app split.
 * Resolve before admission, so a person who can open Profile does not need the
 * administrative Identity or Settings app merely to follow their old link. */
export function appDestination(appId: string, sectionId?: string): { appId: string; sectionId?: string } {
  if (appId === "identity" && ["profile", "devices", "tokens", "settings"].includes(sectionId ?? "")) {
    return { appId: "profile", sectionId };
  }
  if (appId === "settings" && ["about", "appearance", "ask", "apps", "diagnostics"].includes(sectionId ?? "")) {
    return { appId: "profile", sectionId };
  }
  const moved = SECTION_RELOCATIONS.find(move => move.fromApp === appId && move.fromSection === sectionId);
  if (moved) return { appId: moved.toApp, sectionId: moved.toSection };
  // The retired billing-account token was never accepted by the engine.
  if (appId === "accounts" && sectionId === "credentials") return { appId, sectionId: "accounts" };
  return { appId, sectionId };
}
