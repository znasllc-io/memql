import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import type { OsRegistry } from "../../../system/registry";
import type { GrantRow as RoleGrantRow, RoleRow } from "../../users/rows";
import { roleHolds } from "../../users/rows";

// SETTINGS > ACCESS, THE MODEL (epic memql#5289, task memql#5307; design
// section 4 "Administration surface, minimal").
//
// ===========================================================================
// PURE, BECAUSE THE ANSWER IS THE FEATURE
// ===========================================================================
// What this screen shows for one cell is the engine's resolution rule
// (component/auth/grant_resolver.go, design section 1) restated for a
// SUBJECT the viewer chose: the role catalog's answer, overlaid by the
// subject's group grants (deny wins between groups), overlaid by the
// subject's own grants. Most specific wins across levels, deny wins within
// one. It is restated here rather than asked of the engine because
// `effectiveCapabilitiesForActor` answers for the CALLER and nobody else --
// a person's own set is theirs to read, a colleague's is assembled from the
// admin-floored grant reads and the public role catalog. The same three
// inputs produce the same answer either way, and this file has no other
// input to disagree with.
//
// ===========================================================================
// THE ROWS ARE RESOURCES, AND A RESOURCE EXISTS BY BEING SEEDED
// ===========================================================================
// The screen's rows are every `app:*` resource the role catalog names
// (design D7: a part exists by being seeded on at least one role), plus any
// the subject's grants name -- so a deny on something no role holds stays
// visible, or it could never be lifted. Labels come from the OS registry:
// an app's door reads as the app's name, a section as its name under the
// app, a part (`deploy`, `publish`) as the word the seeds use.

/** The engine's five verbs; opening an app is `read`, a part is `execute`. */
export const OPEN_VERB = "read";
export const PART_VERB = "execute";

/** Where an answer came from, in the engine's words. */
export type AnswerSource = "role" | "group" | "user";

/** One grant row (`v1:rbac:grant`), as the two grant reads project it. */
export interface AccessGrant {
  id: string;
  subjectKind: "user" | "group";
  subjectId: string;
  verb: string;
  resource: string;
  effect: "allow" | "deny";
  grantedBy: string;
  active: boolean;
  createdAt: string;
}

export function grantFromRow(raw: Row): AccessGrant {
  const row = flatten(raw);
  const str = (k: string) => (typeof row[k] === "string" ? (row[k] as string).trim() : "");
  return {
    id: str("id"),
    subjectKind: str("subjectKind") === "group" ? "group" : "user",
    subjectId: bareId(str("subjectId")),
    verb: str("verb"),
    resource: str("resourceType"),
    effect: str("effect") === "deny" ? "deny" : "allow",
    grantedBy: bareId(str("grantedBy")),
    // Absent reads as active: the concept's default, and a folded event that
    // omitted the field must not read a live grant as revoked.
    active: row["active"] !== false,
    createdAt: str("createdAt"),
  };
}

/** A user or group id in its bare spelling (the wire form); canonical ids are tolerated. */
export function bareId(id: string): string {
  const trimmed = id.trim();
  const at = trimmed.lastIndexOf(":");
  return at < 0 ? trimmed : trimmed.slice(at + 1);
}

/** The subject the matrix is about. */
export interface Subject {
  kind: "user" | "group";
  id: string;
  name: string;
  /** The subject's role slug (a person); "" for a group, which has none. */
  role: string;
}

/** One resource the matrix has a row for. */
export interface AccessResource {
  /** `app:<id>` or `app:<id>/<part>`. */
  resource: string;
  /** The app id the resource belongs to. */
  appId: string;
  /** "" for the app's own door; the part or section id otherwise. */
  part: string;
  /** The verb that holds it: `read` for a door or a floored section, `execute` for a named part. */
  verb: string;
  /** What to call it: the app's name, or the part's word / the section's name. */
  label: string;
}

/**
 * The resources the catalog names, in registry order: each app's door first,
 * then its parts and floored sections. A resource the catalog names for an
 * app the registry does not have is kept at the end -- a product bundle's
 * app, say -- labelled by its own id rather than dropped.
 */
export function accessResources(
  registry: OsRegistry,
  catalog: readonly RoleGrantRow[],
  extra: readonly string[] = [],
): AccessResource[] {
  const named = new Set<string>();
  for (const row of catalog) if (row.resourceType.startsWith("app:")) named.add(row.resourceType);
  for (const r of extra) if (r.startsWith("app:")) named.add(r);

  const out: AccessResource[] = [];
  const seen = new Set<string>();
  const push = (r: AccessResource) => {
    if (seen.has(r.resource)) return;
    seen.add(r.resource);
    out.push(r);
  };

  const apps = [...registry.apps, ...registry.widgets];
  for (const app of apps) {
    const door = `app:${app.id}`;
    if (!named.has(door)) continue;
    push({ resource: door, appId: app.id, part: "", verb: OPEN_VERB, label: app.name });
    const sections = "sections" in app ? (app.sections ?? []) : [];
    // Parts and sections under this app, in the order the catalog names them
    // (the seeds are ordered app by app, part by part), sections labelled by
    // name where the registry has one.
    for (const resource of [...named].filter((r) => r.startsWith(`${door}/`)).sort(catalogOrder(catalog))) {
      const part = resource.slice(door.length + 1);
      const section = sections.find((s) => s.id === part);
      const isSection = section !== undefined;
      push({
        resource,
        appId: app.id,
        part,
        verb: verbFor(catalog, resource) ?? (isSection ? OPEN_VERB : PART_VERB),
        label: isSection ? section.name : part,
      });
    }
  }
  // Anything the registry does not know, last, by its own name.
  for (const resource of [...named].sort()) {
    if (seen.has(resource)) continue;
    const [door, part = ""] = resource.split("/", 2) as [string, string?];
    push({
      resource,
      appId: door.slice("app:".length),
      part,
      verb: verbFor(catalog, resource) ?? (part === "" ? OPEN_VERB : PART_VERB),
      label: resource,
    });
  }
  return out;
}

function catalogOrder(catalog: readonly RoleGrantRow[]): (a: string, b: string) => number {
  const first = new Map<string, number>();
  catalog.forEach((row, i) => {
    if (!first.has(row.resourceType)) first.set(row.resourceType, i);
  });
  return (a, b) => (first.get(a) ?? Number.MAX_SAFE_INTEGER) - (first.get(b) ?? Number.MAX_SAFE_INTEGER) || a.localeCompare(b);
}

/** The verb the catalog holds a resource under, or undefined when only a grant names it. */
function verbFor(catalog: readonly RoleGrantRow[], resource: string): string | undefined {
  return catalog.find((row) => row.resourceType === resource)?.verb;
}

/** One cell's answer: held or not, and where the answer came from. */
export interface Answer {
  held: boolean;
  source: AnswerSource;
  /** The grant that decided it, when a grant did. */
  grant: AccessGrant | null;
}

/**
 * The engine's rule, for a subject the viewer chose.
 *
 * `groupGrants` is every ACTIVE grant of every group the subject is in (for a
 * person), or empty (for a group, which is not in groups). `ownGrants` is the
 * subject's own. A group subject has no role level: its "inherited" answer
 * is "nothing of its own", because a group grants apps and nothing else.
 */
export function resolveAnswer(
  subject: Subject,
  verb: string,
  resource: string,
  catalog: readonly RoleGrantRow[],
  groupGrants: readonly AccessGrant[],
  ownGrants: readonly AccessGrant[],
): Answer {
  let answer: Answer = {
    held: subject.kind === "user" && subject.role !== "" ? roleHolds(catalog, subject.role, verb, resource) : false,
    source: "role",
    grant: null,
  };
  const groupLevel = overlay(groupGrants, verb, resource);
  if (groupLevel !== null) answer = { held: groupLevel.effect === "allow", source: "group", grant: groupLevel };
  const ownLevel = overlay(ownGrants, verb, resource);
  if (ownLevel !== null) answer = { held: ownLevel.effect === "allow", source: "user", grant: ownLevel };
  return answer;
}

/** Deny wins within a level; the deciding grant is returned, null when the level is silent. */
function overlay(grants: readonly AccessGrant[], verb: string, resource: string): AccessGrant | null {
  let decided: AccessGrant | null = null;
  for (const g of grants) {
    if (!g.active || g.verb !== verb || g.resource !== resource) continue;
    if (g.effect === "deny") return g;
    decided = g;
  }
  return decided;
}

/** The catalog slugs whose role holds `verb` on `resource`, in the ladder's order (strongest first). */
export function rolesHolding(
  roles: readonly RoleRow[],
  catalog: readonly RoleGrantRow[],
  verb: string,
  resource: string,
): RoleRow[] {
  return [...roles]
    .sort((a, b) => b.rank - a.rank || a.slug.localeCompare(b.slug))
    .filter((role) => roleHolds(catalog, role.slug, verb, resource));
}

// ---------------------------------------------------------------------------
// The refusal copy
// ---------------------------------------------------------------------------

/** What the engine says when a grant write is refused: the code, and the sentence. */
export interface GrantRefusal {
  code: string;
  message: string;
}

export interface RefusalCopy {
  title: string;
  next: string;
}

/**
 * Every governance code the two builtins emit (integrations/rbac/grants.go),
 * with a headline and the next move. The server's own sentence renders
 * beneath, verbatim. A code this build does not know renders the sentence
 * under a neutral heading -- never a guessed one.
 */
const COPY: Record<string, RefusalCopy> = {
  grant_caller_not_permitted: {
    title: "Your role does not write grants",
    next: "A grant is written by a role holding update on principal. An owner can give yours that in Roles.",
  },
  grant_capability_not_held: {
    title: "You cannot grant what you do not hold",
    next: "Nobody hands out, or denies, an app they do not have themselves. Ask somebody who holds it.",
  },
  grant_subject_outranks_caller: {
    title: "They rank above you",
    next: "A grant reaches down the ladder, never up. A group ranks as its highest member, so a group with an owner in it is the owner's to decide.",
  },
  grant_self: {
    title: "Not for yourself",
    next: "Nobody grants to themselves, the way nobody adds themselves to a group. Ask a colleague who holds it.",
  },
  grant_unknown_subject: {
    title: "That person or group is not on this cluster",
    next: "Pick them again from the list; the row they had may have been deactivated.",
  },
  grant_invalid: {
    title: "That is not a grant this cluster can write",
    next: "",
  },
  grant_not_found: {
    title: "That grant is already gone",
    next: "Somebody revoked it first. The matrix re-reads on focus, so it is current now.",
  },
};

export function grantRefusalCopy(code: string): RefusalCopy | null {
  return COPY[code.trim()] ?? null;
}

/** Every code this build renders copy for, for the coverage test. */
export function knownGrantCodes(): string[] {
  return Object.keys(COPY).sort();
}
