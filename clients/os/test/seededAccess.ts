import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { setEffectiveCapabilities, type EffectiveCapability } from "../src/system/roles";
import { SEEDED_LADDER } from "./seededLadder";

/**
 * The capability set each seeded role holds, READ FROM THE SEEDS THEMSELVES.
 *
 * jsdom has no cluster to ask `effectiveCapabilitiesForActor`, and the shell
 * draws nothing gated until that answer is in (epic memql#5289, D10). So a
 * suite that renders as a role installs that role's set here first -- and the
 * set comes from `dsl/rbac/seeds.memql` rather than from a literal, because a
 * literal is a second copy of the policy in a different language, which is the
 * exact defect the switch to `requires:` removed from the registry.
 *
 * WHAT IT ANSWERS IS THE ENGINE'S ROLE LEVEL AND NOTHING ELSE: no group
 * grants, no user grants. That is what a fresh cluster resolves for every
 * person, and it is what the parity gate (component/memql,
 * TestOsRegistryRequiresMatchTheAppSeeds) promised the switch would preserve.
 * A test about a GRANT installs its own entries on top.
 */

export interface SeededCapability {
  roleSlug: string;
  verb: string;
  resource: string;
  effect: "allow" | "deny";
}

const here = dirname(fileURLToPath(import.meta.url));
const SEEDS_PATH = join(here, "..", "..", "..", "dsl", "rbac", "seeds.memql");

const CAPABILITY_LINE =
  /seed capability [\w-]+ \{\s*roleSlug:\s*"([^"]+)"\s+verb:\s*"([^"]+)"\s+resourceType:\s*"([^"]+)"(?:\s+effect:\s*"([^"]+)")?/g;

function readSeededCapabilities(): SeededCapability[] {
  const raw = readFileSync(SEEDS_PATH, "utf8");
  const out: SeededCapability[] = [];
  for (const m of raw.matchAll(CAPABILITY_LINE)) {
    out.push({
      roleSlug: m[1] ?? "",
      verb: m[2] ?? "",
      resource: m[3] ?? "",
      effect: m[4] === "deny" ? "deny" : "allow",
    });
  }
  if (out.length === 0) {
    throw new Error(`read no capability rows out of ${SEEDS_PATH}; the seeds moved or changed shape`);
  }
  return out;
}

/** Every seeded (role, verb, resource) row, read once. */
export const SEEDED_CAPABILITIES: readonly SeededCapability[] = readSeededCapabilities();

/** A role slug or one of its aliases, resolved to the catalog slug. */
export function catalogSlug(role: string): string {
  const slug = role.trim();
  for (const rung of SEEDED_LADDER) {
    if (rung.slug === slug || rung.aliases.includes(slug)) return rung.slug;
  }
  return slug;
}

/**
 * The effective set a person holding `role` resolves to on a cluster with no
 * grants: the role's seeded rows, deny winning within the level.
 */
export function seededAccessFor(role: string): EffectiveCapability[] {
  const slug = catalogSlug(role);
  const byPair = new Map<string, EffectiveCapability>();
  for (const cap of SEEDED_CAPABILITIES) {
    if (cap.roleSlug !== slug) continue;
    const key = `${cap.verb} ${cap.resource}`;
    const held = byPair.get(key);
    if (held !== undefined && held.effect === "deny") continue;
    byPair.set(key, { verb: cap.verb, resource: cap.resource, effect: cap.effect, source: "role" });
  }
  return [...byPair.values()];
}

/** Whether `role`'s seeded set opens a surface naming `resource`. */
export function roleOpens(role: string, resource: string): boolean {
  return seededAccessFor(role).some((c) => c.verb === "read" && c.resource === resource && c.effect === "allow");
}

/** The catalog slugs whose seeded set holds `read` on `resource`, by rank descending. */
export function rolesOpening(resource: string): string[] {
  return [...SEEDED_LADDER]
    .sort((a, b) => b.rank - a.rank)
    .map((r) => r.slug)
    .filter((slug) => roleOpens(slug, resource));
}

/** Every `read app:*` resource the seeds name, for the parity tests. */
export function seededAppResources(): string[] {
  return [
    ...new Set(
      SEEDED_CAPABILITIES.filter((c) => c.verb === "read" && c.resource.startsWith("app:")).map(
        (c) => c.resource,
      ),
    ),
  ].sort();
}

/**
 * Install `role`'s seeded set as the shell's effective set.
 *
 * No restorer is returned on purpose: test/setup.ts installs the default
 * before every file, and a test that changes the role calls this again with
 * the one it wants. A suite that needs the PRE-READ state calls
 * `clearEffectiveCapabilities` itself.
 */
export function installSeededAccess(role: string): void {
  setEffectiveCapabilities(seededAccessFor(role));
}
