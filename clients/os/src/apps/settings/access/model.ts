import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import type { OsRegistry } from "../../../system/registry";
import type { GroupRow, GrantRow as RoleGrantRow, RoleRow } from "../../users/rows";
import { roleHolds } from "../../users/rows";

// SETTINGS > ACCESS, THE MODEL (epic memql#5289, task memql#5307; design
// section 4 "Administration surface, minimal"; access clarity, 2026-11-06).
//
// ===========================================================================
// THE ANSWER IS THE ENGINE'S; THIS FILE SAYS IT
// ===========================================================================
// What a person can open, and why, comes from the engine's read
// `effectiveAccessForSubject` (integrations/rbac/grants.go): the resolver the
// gates call, run for the person named -- their role, then their groups'
// grants, then grants to them by name, an organization role's limits, and the
// answers the engine makes per organization. For a group, the read answers
// the group's own grants.
//
// This file used to restate that rule in TypeScript, from the role catalog
// and the grant rows, because the only resolved read answered for the caller
// alone. The restatement could not see an organization role's limits or the
// per-organization answers, so an organization's moderator read as able to
// open every app. Nothing here decides an answer now. It reads the rows,
// joins them where the shell itself joins them (an organization's door opens
// there only where that organization's data can be read too, and a part of
// it only through that door, with the data changeable too), says each answer
// in words, and works out which acts could change one.
//
// ===========================================================================
// THE ROWS ARE RESOURCES, AND A RESOURCE EXISTS BY BEING SEEDED
// ===========================================================================
// The screen's rows are every `app:*` resource the role catalog names
// (design D7: a part exists by being seeded on at least one role), plus any
// the chosen subject's grants or the engine's answers for them name. A row
// asks one verb: the catalog's, or for a resource the catalog does not name,
// `read` for a door or a section and `execute` for any other part. Labels
// come from the OS registry: an app's door reads as the app's name, a section
// as its name under the app, a part (`deploy`, `publish`) as the word the
// seeds use.
//
// WHERE A PAIR THE CATALOG DOES NOT NAME SHOWS. A person's row is drawn only
// where the engine answered the row's own pair. The engine answers every pair
// the person's grants and their groups' grants name, as well as the catalog's
// (component/auth, EffectiveCapabilities), so a deny on something no role
// holds keeps its row under By person, and its "Use role". A grant under a
// verb its resource's row does not ask -- `execute` on a door, `read` on a
// part -- is answered under that verb and drawn by no row: it is seen, and
// revoked, under By app only, whose holders are every grant on the resource
// whatever its verb. By app offers a resource the catalog does not name only
// while a subject whose grants or answers name it is chosen. A group's row is
// drawn for every resource, from its own grant on the row's pair or none, so
// its grant under another verb reads there as "Not granted by this group".

/** The engine's five verbs; opening an app is `read`, a part is `execute`. */
export const OPEN_VERB = "read";
export const PART_VERB = "execute";

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

// ---------------------------------------------------------------------------
// The engine's answer
// ---------------------------------------------------------------------------

/**
 * Where the engine says an answer came from (integrations/rbac/grants.go,
 * `accessRowFrom`). `role` held is the role's own answer; `role` NOT held is a
 * global role's limit that no grant lifts (Fleet below the roles that manage
 * it). "" is a source this build does not know, said as no source at all.
 */
export type AccessSource = "role" | "group" | "person" | "scoped-role-limit" | "not-granted" | "";

const SOURCES: readonly string[] = ["role", "group", "person", "scoped-role-limit", "not-granted"];

/** One row of `effectiveAccessForSubject`: one pair, answered once or in one organization. */
export interface AccessRow {
  verb: string;
  resource: string;
  held: boolean;
  source: AccessSource;
  /** The role's slug, the group's id or the person's id, bare; "" when nothing granted it. */
  sourceId: string;
  /** "" for a pair answered once; the organization's bare account id for a per-organization answer. */
  accountId: string;
}

export function accessRowFrom(raw: Row): AccessRow {
  const row = flatten(raw);
  const str = (k: string) => (typeof row[k] === "string" ? (row[k] as string).trim() : "");
  const source = str("source");
  return {
    verb: str("verb"),
    resource: str("resourceType"),
    // EITHER SPELLING. A scalar boolean has crossed a builtin's reply row as
    // the STRING "true" (apps/deployables/sources/calls.ts); reading only the
    // boolean would draw every door this person can open as shut.
    held: row["held"] === true || row["held"] === "true",
    source: SOURCES.includes(source) ? (source as AccessSource) : "",
    sourceId: bareId(str("sourceId")),
    accountId: bareId(str("accountId")),
  };
}

/** The organization's records, which its doors read and its parts change. */
const DATA_RESOURCE = "data";

/**
 * What an answer in an organization needs besides the pair itself, as the
 * shell asks it; "" when the pair is the whole answer.
 *
 *   - `data-read`:   a door, and that organization's data cannot be read.
 *   - `door`:        a part, and its app's door there is shut (not held, or
 *                    its data cannot be read).
 *   - `data-update`: a part whose door is open, and that organization's data
 *                    cannot be changed.
 */
export type Missing = "" | "data-read" | "door" | "data-update";

/** One answer a person meets for one pair: once, or in one organization. */
export interface PersonAnswer {
  /** "" for a pair answered once; the organization's bare account id otherwise. */
  accountId: string;
  /** What the person meets: the pair held, and nothing else missing there. */
  canOpen: boolean;
  /** What else the answer needs in that organization; "" when nothing. */
  missing: Missing;
  /** The engine's row for the pair itself, which says where the answer came from. */
  row: AccessRow;
}

/**
 * A person's answers for one resource: one, or one per organization the
 * engine answered it in.
 *
 * IN AN ORGANIZATION, THE PAIR ALONE IS NOT WHAT THE SHELL OPENS. An
 * organization's door -- Email, Deployables -- opens there only where `read
 * data` holds too (system/roles.ts, `availableInAnyOrganization`). A part of
 * one is drawn there only through that door, with the data read AND `update
 * data` holding there (apps/deployables/parts.ts, `partsForOrganization`), so
 * a part the role holds is no answer while its door is shut. Each
 * organization is joined on its own rows and never on another's. The shell
 * asks an organization's own answers only (`holdsForOrganization`), so a door
 * the engine answers once, not per organization, does not open a part there.
 * A pair answered once (an operator's, or a person whose scope reaches no
 * organization) is the shell's global answer and needs nothing else.
 */
export function personAnswers(rows: readonly AccessRow[], resource: AccessResource): PersonAnswer[] {
  const heldIn = (verb: string, pair: string, accountId: string) =>
    rows.some((r) => r.verb === verb && r.resource === pair && r.accountId === accountId && r.held);
  const missingIn = (accountId: string): Missing => {
    const dataRead = heldIn(OPEN_VERB, DATA_RESOURCE, accountId);
    if (resource.part === "") return dataRead ? "" : "data-read";
    if (!dataRead || !heldIn(OPEN_VERB, `app:${resource.appId}`, accountId)) return "door";
    return heldIn("update", DATA_RESOURCE, accountId) ? "" : "data-update";
  };
  return rows
    .filter((r) => r.verb === resource.verb && r.resource === resource.resource)
    .map((row) => {
      const missing = row.accountId === "" ? "" : missingIn(row.accountId);
      return { accountId: row.accountId, canOpen: row.held && missing === "", missing, row };
    });
}

/** The acts that could change an answer for one pair, and the grant "Use role" or "Remove" revokes. */
export interface AccessActs {
  allow: boolean;
  deny: boolean;
  /** The subject's own active grant on the pair; null when they hold none. */
  own: AccessGrant | null;
}

const NO_ACTS: AccessActs = { allow: false, deny: false, own: null };

/**
 * The acts a person's answers admit (DESIGN.md rule 12: an act that is not
 * legal is ABSENT, never disabled). An act that cannot change the answer is
 * not legal here:
 *
 *   - A pair a role's limit bars -- an organization role's, or a global
 *     role's -- offers nothing: no grant lifts it, and a grant already there
 *     changes nothing either.
 *   - Deny only where some answer is "can open"; Allow only where it would
 *     make one so: the pair is shut there and nothing else is missing. That
 *     is also why neither is offered over the person's own grant that already
 *     says it: theirs decides last, in every organization, so their allow
 *     leaves nothing shut that a grant could open and their deny leaves
 *     nothing open.
 *   - "Use role" only over the person's own grant on the pair, which it
 *     revokes; its id comes from the grant read, since the engine's rows
 *     carry none.
 *
 * ONCE PER PAIR. A grant to a person counts in every organization they are
 * in, so the acts belong to the pair and never to one organization's answer,
 * and an act is withheld only when it changes nothing in ANY of them: a
 * part's door shut in Beta does not take away the Deny that closes it in
 * Acme.
 */
export function personActs(answers: readonly PersonAnswer[], ownGrants: readonly AccessGrant[], resource: AccessResource, subject: Subject): AccessActs {
  const barred = answers.some((a) => a.row.source === "scoped-role-limit" || (a.row.source === "role" && !a.row.held));
  if (answers.length === 0 || barred) return NO_ACTS;
  return {
    allow: answers.some((a) => !a.canOpen && a.missing === ""),
    deny: answers.some((a) => a.canOpen),
    own: ownGrant(ownGrants, resource, subject),
  };
}

/** A group's own answer for one pair: its grant's effect, or null when it holds none. */
export type GroupAnswer = "allow" | "deny" | null;

export function groupAnswer(rows: readonly AccessRow[], resource: AccessResource): GroupAnswer {
  const row = rows.find((r) => r.verb === resource.verb && r.resource === resource.resource);
  return row === undefined ? null : row.held ? "allow" : "deny";
}

/**
 * The acts a group's answer admits: Allow wherever the group does not already
 * allow, Deny wherever it does not already deny, and "Remove" over a grant it
 * holds. A group that grants nothing can do either -- an allow widens what its
 * people's roles give them, a deny narrows it -- so "Not granted by this
 * group" offers both. A group has no role, so there is no "Use role".
 */
export function groupActs(answer: GroupAnswer, ownGrants: readonly AccessGrant[], resource: AccessResource, subject: Subject): AccessActs {
  return {
    allow: answer !== "allow",
    deny: answer !== "deny",
    own: answer === null ? null : ownGrant(ownGrants, resource, subject),
  };
}

/** One app a group opens for its people, the parts of it the group allows, and the grant each "Remove" takes. */
export interface GroupApp {
  door: AccessResource;
  /** The group's own grant on the door; null until the grant read has answered for this group. */
  grant: AccessGrant | null;
  parts: { part: AccessResource; grant: AccessGrant | null }[];
}

/**
 * The apps a group's ALLOW grants open for its people, in the list's order:
 * each door the group allows, with what it allows inside that app beneath.
 * A deny opens nothing, so it is not here (Settings > Access shows it, where
 * it can be lifted), and nor is a part whose door the group does not open.
 * The grant a "Remove" revokes is the group's own on that pair, from the grant
 * read: the engine's rows carry no grant id.
 */
export function groupApps(rows: readonly AccessRow[], resources: readonly AccessResource[], ownGrants: readonly AccessGrant[], subject: Subject): GroupApp[] {
  const allows = (r: AccessResource) => groupAnswer(rows, r) === "allow";
  return resources
    .filter((r) => r.part === "" && r.verb === OPEN_VERB && allows(r))
    .map((door) => ({
      door,
      grant: ownGrant(ownGrants, door, subject),
      parts: resources
        .filter((r) => r.appId === door.appId && r.part !== "" && allows(r))
        .map((part) => ({ part, grant: ownGrant(ownGrants, part, subject) })),
    }));
}

/** The catalog's apps a group does not already open for its people: what "Add an app" offers. */
export function appsToAdd(rows: readonly AccessRow[], resources: readonly AccessResource[]): AccessResource[] {
  return resources.filter((r) => r.part === "" && r.verb === OPEN_VERB && groupAnswer(rows, r) !== "allow");
}

/**
 * The subject's own active grant on the pair: THEIRS, by kind and bare id, as
 * well as on the pair. A revoke acts on the id it is handed, so a grant read
 * that answered for somebody else must never supply one.
 */
function ownGrant(grants: readonly AccessGrant[], resource: AccessResource, subject: Subject): AccessGrant | null {
  return (
    grants.find(
      (g) =>
        g.active &&
        g.subjectKind === subject.kind &&
        bareId(g.subjectId) === bareId(subject.id) &&
        g.verb === resource.verb &&
        g.resource === resource.resource,
    ) ?? null
  );
}

// ---------------------------------------------------------------------------
// The answer in words
// ---------------------------------------------------------------------------

/** The names an answer is said in. Each falls back to the bare id it was given, never to nothing. */
export interface AccessNames {
  role: (slug: string) => string;
  group: (groupId: string) => string;
  organization: (accountId: string) => string;
  /** An app's name by its id, as its door is labelled in the list. */
  app: (appId: string) => string;
}

/**
 * Names from the reads the screen already has: the roles, the groups -- an
 * organization's own group carries the organization's name
 * (integrations/groups/ensure.go keeps the two in step) -- and the list's
 * own resources, whose doors carry the apps' names.
 */
export function accessNames(roles: readonly RoleRow[], groups: readonly GroupRow[], resources: readonly AccessResource[]): AccessNames {
  return {
    role: (slug) => roles.find((r) => r.slug === slug || r.aliases.includes(slug))?.name || slug,
    group: (groupId) => groups.find((g) => bareId(g.id) === bareId(groupId))?.name || bareId(groupId),
    organization: (accountId) =>
      groups.find((g) => g.kind === "account" && bareId(g.accountId) === bareId(accountId))?.name || bareId(accountId),
    app: (appId) => resources.find((r) => r.appId === appId && r.part === "")?.label || appId,
  };
}

/** An answer as a person reads it: the verdict, then where it came from ("" when nothing says). */
export interface AnswerWords {
  verdict: string;
  reason: string;
}

/** "Can open in Acme", "from the group Acme". */
export function personAnswerWords(answer: PersonAnswer, names: AccessNames): AnswerWords {
  const where = answer.accountId === "" ? "" : ` in ${names.organization(answer.accountId)}`;
  const from = sourceWords(answer.row, names);
  const missing = missingWords(answer, names);
  // What else is missing is the reason when the pair itself holds; when it
  // does not, both are said, so an absent Allow is never left unexplained.
  let reason = from;
  if (missing !== "") reason = answer.row.held || from === "" ? missing : `${from} and ${missing}`;
  return { verdict: `${answer.canOpen ? "Can open" : "Can't open"}${where}`, reason };
}

function missingWords(answer: PersonAnswer, names: AccessNames): string {
  switch (answer.missing) {
    case "data-read":
      return "they can't read its data";
    case "door":
      return `they can't open ${names.app(appIdOf(answer.row.resource))} there`;
    case "data-update":
      return "they can't change its data";
    default:
      return "";
  }
}

/** `app:deployables/deploy` -> `deployables`. */
function appIdOf(resource: string): string {
  return resource.replace(/^app:/, "").split("/")[0] ?? "";
}

function sourceWords(row: AccessRow, names: AccessNames): string {
  switch (row.source) {
    case "role":
      return row.held ? `from the role ${names.role(row.sourceId)}` : `the role ${names.role(row.sourceId)} can't open this`;
    case "group":
      return row.held ? `from the group ${names.group(row.sourceId)}` : `denied by the group ${names.group(row.sourceId)}`;
    case "person":
      return row.held ? "granted to this person" : "denied to this person";
    case "scoped-role-limit":
      return "an organization role can't open this";
    case "not-granted":
      return "nothing grants it";
    default:
      return "";
  }
}

/** A group's own answer, in words. */
export function groupAnswerWords(answer: GroupAnswer): string {
  if (answer === "allow") return "Granted";
  if (answer === "deny") return "Denied";
  return "Not granted by this group";
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
