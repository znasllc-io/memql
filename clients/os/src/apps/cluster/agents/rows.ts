import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten, stringsOf } from "../../../kit";

// Reading an agent row, and deciding what counts as news about one.

export interface AgentRow {
  id: string;
  name: string;
  description: string;
  kind: string;
  role: string;
  roleSlug: string;
  ownerUserId: string;
  active: boolean;
  capabilities: string[];
  groupIds: string[];
  createdAt: string;
}

export function agentFromRow(raw: Row): AgentRow {
  const row = flatten(raw);
  return {
    id: stringOf(row, "id"),
    name: stringOf(row, "name"),
    description: stringOf(row, "description"),
    kind: stringOf(row, "kind"),
    role: stringOf(row, "role"),
    roleSlug: stringOf(row, "roleSlug"),
    ownerUserId: stringOf(row, "ownerUserId"),
    // `active` defaults TRUE on the concept and a folded CDC event carries
    // only what the write touched, so a bare truthiness read would turn every
    // unrelated update into a deactivation.
    active: boolOr(row, "active", true),
    capabilities: capabilityNames(row["capabilities"]),
    groupIds: stringsOf(row, "groupIds"),
    createdAt: stringOf(row, "createdAt"),
  };
}

/**
 * THE ARRIVAL CUE'S CONTRACT (clients/os/README.md, "A HEARTBEAT IS NOT
 * NEWS").
 *
 * Anything named here announces itself when it changes, so what is named has
 * to be what a PERSON would call a change to an agent: its name, whether it
 * is active, what it can do, which groups it is in, what role it plays.
 *
 * `createdAt` IS THIS CONCEPT'S HEARTBEAT, and it is the reason this comment
 * is long. An agent row is append-only and the SeedMaterializer re-writes
 * every system agent -- MemQL Planner, MemQL Trainer -- on EVERY boot, so
 * `createdAt` moves for those rows on a schedule nobody set, with nothing
 * about the agent having changed. Naming it would ring the list every time a
 * replica restarted.
 *
 * `systemPrompt` and `providerConfig` are left out for the milder version of
 * the same reason: they are configuration this list does not show, they are
 * re-stamped by seeding and by provider reloads, and a ring that points at a
 * row whose visible content is identical is a cue with nothing behind it.
 */
export function agentFingerprint(agent: AgentRow): string {
  return [
    agent.name,
    String(agent.active),
    agent.kind,
    agent.role,
    agent.roleSlug,
    agent.capabilities.join(","),
    agent.groupIds.join(","),
  ].join("|");
}

/**
 * The capability list, from a field the concept declares as an OBJECT rather
 * than a string array.
 *
 * Three shapes reach here and all three are real: a plain string array (what
 * a shape projection of a `[]string` field gives), an object whose values are
 * arrays (the `capabilities { skillIds, domains, tools }` block), and
 * absent. Anything else yields an empty list rather than a guess -- a
 * capability list is what an agent may DO, and inventing an entry for it is
 * the wrong direction to be wrong in.
 */
export function capabilityNames(raw: unknown): string[] {
  if (Array.isArray(raw)) return raw.filter((m): m is string => typeof m === "string");
  if (raw !== null && typeof raw === "object") {
    const out: string[] = [];
    for (const value of Object.values(raw as Record<string, unknown>)) {
      if (Array.isArray(value)) {
        for (const member of value) if (typeof member === "string") out.push(member);
      }
    }
    return out;
  }
  return [];
}

/** One standing grant, as the caller's own. */
export interface AuthorizationRow {
  id: string;
  agentId: string;
  userId: string;
  planKind: string;
  spaceScope: string;
  computerUseScope: string;
  tokenBudgetCap: number | null;
  expiresAt: string;
  active: boolean;
}

export function authorizationFromRow(raw: Row): AuthorizationRow {
  const row = flatten(raw);
  const cap = row["tokenBudgetCap"];
  return {
    id: stringOf(row, "id"),
    agentId: stringOf(row, "agentId"),
    userId: stringOf(row, "userId"),
    planKind: stringOf(row, "planKind"),
    spaceScope: stringOf(row, "spaceScope"),
    computerUseScope: stringOf(row, "computerUseScope"),
    // A null cap means "use the user's default budget", which is a different
    // statement from a cap of zero. Kept apart rather than coalesced.
    tokenBudgetCap: typeof cap === "number" && Number.isFinite(cap) ? cap : null,
    expiresAt: stringOf(row, "expiresAt"),
    active: boolOr(row, "active", true),
  };
}

/** `interact` is a RETIRED tier kept in the enum for rows written before the
 *  simplification, and the read path treats it as `full`. The surface says so
 *  rather than showing a word no new grant can carry. */
export function computerUseScopeReading(scope: string): string {
  if (scope === "") return "none";
  if (scope === "interact") return "full (recorded as the retired 'interact')";
  return scope;
}

function stringOf(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}
