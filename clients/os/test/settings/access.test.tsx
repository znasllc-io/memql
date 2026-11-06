import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { AccessSection } from "../../src/apps/settings/AccessSection";
import { accessResources, knownGrantCodes, type AccessGrant } from "../../src/apps/settings/access/model";
import { OS_REGISTRY } from "../../src/apps/registry";
import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { onGrantWritten } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { installSeededAccess, SEEDED_CAPABILITIES } from "../seededAccess";
import { chooseOption } from "../selectControl";
import { SEEDED_LADDER } from "../seededLadder";

// SETTINGS > ACCESS (epic memql#5289, task memql#5307; access clarity,
// 2026-11-06). By person, the answers are the ENGINE's
// (`effectiveAccessForSubject`), said in words with where each one comes from
// and, where the engine answers per organization, which organization. By
// group, the group's own grants. The acts are the kit's buttons in the row's
// act column, offered only where they change an answer, and a read the engine
// refuses is a notice in place of the answers. By app: the roles that hold
// something and everyone granted it by name.

const ME: Row = { id: "u-me", displayName: "Owner", primaryEmail: "owner@example.com", role: "owner", active: true } as Row;
const ADA: Row = { id: "u-ada", displayName: "Ada Lovelace", primaryEmail: "ada@example.com", role: "developer", active: true } as Row;
/** A writer in two organizations. */
const MAX: Row = { id: "u-max", displayName: "Max Planck", primaryEmail: "max@example.com", role: "writer", active: true } as Row;
/** An organization's moderator: a role scoped to Acme. */
const JANE: Row = { id: "u-jane", displayName: "Jane Doe", primaryEmail: "jane@example.com", role: "acme-moderator", active: true } as Row;
const ACME: Row = { id: "g-acme", name: "Acme", description: "", kind: "account", accountId: "acct-7f3a", status: "active", createdAt: "2026-09-01T00:00:00Z" } as Row;
const BETA: Row = { id: "g-beta", name: "Beta", description: "", kind: "account", accountId: "acct-2b9c", status: "active", createdAt: "2026-09-01T00:00:00Z" } as Row;
const ACME_MODERATOR: Row = { id: "role-acme-moderator", slug: "acme-moderator", name: "Acme moderator", rank: 20, description: "", predefined: false, active: true, aliases: [], accountId: "acct-7f3a" } as Row;
const ACME_ADMIN: Row = { id: "role-acme-admin", slug: "acme-admin", name: "Acme admin", rank: 200, description: "", predefined: false, active: true, aliases: [], accountId: "acct-7f3a" } as Row;

/** The seeded catalog, as `activeCapabilities` answers it. */
function catalogRows(): Row[] {
  return SEEDED_CAPABILITIES.map((c, i) => ({
    id: `cap-${i}`,
    roleSlug: c.roleSlug,
    verb: c.verb,
    resourceType: c.resource,
    effect: c.effect,
    predefined: true,
    active: true,
  })) as Row[];
}

function ladderRows(): Row[] {
  return SEEDED_LADDER.map((r) => ({ id: `role-${r.slug}`, ...r, active: true, predefined: true })) as Row[];
}

function grantRow(over: Partial<AccessGrant> & { id: string }): Row {
  return {
    subjectKind: "user",
    subjectId: "u-ada",
    verb: "execute",
    resourceType: over.resource ?? "app:deployables/publish",
    effect: "deny",
    grantedBy: "u-me",
    active: true,
    createdAt: "2026-09-10T00:00:00Z",
    ...over,
  } as Row;
}

function membership(groupId: string, userId: string): Row {
  return { id: `m-${groupId}-${userId}`, groupId, userId, status: "active", origin: "added", createdAt: "" } as Row;
}

let accessSeq = 0;

/** One row of `effectiveAccessForSubject`, shaped as the read answers it. */
function accessRow(over: { resource: string; verb?: string; held: boolean; source: string; sourceId?: string; accountId?: string }): Row {
  accessSeq += 1;
  return {
    id: `integration:rbac:effectiveAccess:${String(accessSeq).padStart(5, "0")}`,
    concept: "integration:rbac:effectiveAccess",
    verb: over.verb ?? "read",
    resourceType: over.resource,
    held: over.held,
    source: over.source,
    sourceId: over.sourceId ?? "",
    accountId: over.accountId ?? "",
  } as Row;
}

interface State {
  /** The roster `searchUsers` answers; the two bare rows unless a case says otherwise. */
  people: Row[];
  groups: Row[];
  roles: Row[];
  /** What `effectiveAccessForSubject` answers, by `<kind>:<id>`: the rows, the engine's refusal, or no answer yet. */
  access: Record<string, Row[] | { refusal: string } | "pending">;
  grantsBySubject: Record<string, Row[]>;
  grantsByResource: Record<string, Row[]>;
  memberships: Record<string, Row[]>;
  /** The admin-floored grant and membership reads refuse this viewer too. */
  grantReadsRefused: boolean;
  setReply: Row;
  revokeReply: Row;
  calls: { name: string; call: string }[];
}

function fakeConnection(state: State) {
  const argOf = (call: string, name: string): string => new RegExp(`${name}:\\s*"([^"]*)"`).exec(call)?.[1] ?? "";
  const stub = {
    executeNamed: vi.fn(async (name: string, call: string) => {
      state.calls.push({ name, call });
      const rows = (r: Row[]) => ({ rows: () => r, meta: () => null });
      switch (name) {
        case "searchUsers":
          return rows(state.people);
        case "groupsAll":
          return rows(state.groups);
        case "activeRoles":
          return rows(state.roles);
        case "activeCapabilities":
          return rows(catalogRows());
        case "effectiveAccessForSubject": {
          const answer = state.access[`${argOf(call, "subjectKind")}:${argOf(call, "subjectId")}`] ?? [];
          if (answer === "pending") return new Promise<never>(() => {});
          // What QueryClient.executeNamed throws for a refused builtin: the
          // call's name, around the server's frame, around the engine's code.
          if (!Array.isArray(answer)) throw new Error(`${name}: MemQL engine failed to execute query. Details: ${answer.refusal}`);
          return rows(answer);
        }
        case "grantsForSubject":
          if (state.grantReadsRefused) throw new Error(`${name}: MemQL engine failed to execute query. Details: below the rank this query requires`);
          return rows(state.grantsBySubject[`${argOf(call, "subjectKind")}:${argOf(call, "subjectId")}`] ?? []);
        case "groupsForUser":
          if (state.grantReadsRefused) throw new Error(`${name}: MemQL engine failed to execute query. Details: below the rank this query requires`);
          return rows(state.memberships[argOf(call, "userId")] ?? []);
        case "grantsForResource":
          return rows(state.grantsByResource[argOf(call, "resourceType")] ?? []);
        case "grantSet":
          return rows([state.setReply]);
        case "grantRevoke":
          return rows([state.revokeReply]);
        default:
          return rows([]);
      }
    }),
  };
  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    dispatcher: { sendAndWait: vi.fn() },
    subscriptions: { subscribeGraph: () => () => {} },
  };
}

function freshState(over: Partial<State> = {}): State {
  return {
    people: [ME, ADA],
    groups: [ACME],
    roles: ladderRows(),
    access: {},
    grantsBySubject: {},
    grantsByResource: {},
    memberships: {},
    grantReadsRefused: false,
    setReply: { ok: true, grantId: "grant-new", code: "", message: "" } as Row,
    revokeReply: { ok: true, grantId: "grant-1", code: "", message: "" } as Row,
    calls: [],
    ...over,
  };
}

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function mount(state: State, role = "owner", userId = "u-me") {
  installSeededAccess(role);
  h.connection = fakeConnection(state);
  return render(
    <SessionProvider
      value={{
        access: { userId, primaryEmail: "owner@example.com", role, roleName: "", rank: 0 },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" },
        accessEpoch: 1,
      }}
    >
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }} store={new LocalDesktopStore(memStorage())}>
        <AccessSection />
      </OsProvider>
    </SessionProvider>,
  );
}

/** Pick a person or a group by the name a person reads in the list. */
async function pickSubject(name: string): Promise<void> {
  const trigger = await screen.findByLabelText("Person or group");
  // The roster arrives asynchronously, and the option does not exist until
  // it has; outside `act`, so the click's own act flushes the open state.
  await waitFor(() => chooseOption(trigger, name));
}

/** The row for one app or part: its name, its answers and its acts. */
function rowFor(resource: string): HTMLElement {
  const row = document.querySelector(`[data-resource="${resource}"]`);
  if (row === null) throw new Error(`no row for ${resource}`);
  return row as HTMLElement;
}

/** What one row says, answer by answer, as a person reads it. */
function answersOf(resource: string): string[] {
  return [...rowFor(resource).querySelectorAll(".os-access-answer")].map((el) => (el.textContent ?? "").replace(/\s+/g, " ").trim());
}

/** The acts one row offers, by accessible name, in no particular order. */
function actsOf(resource: string): string[] {
  return within(rowFor(resource))
    .queryAllByRole("button")
    .map((b) => b.getAttribute("aria-label") ?? b.textContent ?? "")
    .sort();
}

/** Turn to the by-app view, pick a resource, and answer its holder list. */
async function pickResource(label: string): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole("button", { name: "By app" }));
  const trigger = await screen.findByLabelText("App or part");
  await waitFor(() => chooseOption(trigger, label));
  return screen.findByRole("list", { name: "Granted by name" });
}

beforeEach(() => {
  h.connection = null;
});

afterEach(() => {
  installSeededAccess("owner");
});

describe("the pure half", () => {
  it("lists every app the catalog names, its parts under it, labelled from the registry", () => {
    const catalog = catalogRows().map((r) => ({
      id: String(r.id),
      roleSlug: String(r.roleSlug),
      verb: String(r.verb),
      resourceType: String(r.resourceType),
      effect: "allow" as const,
      predefined: true,
      active: true,
    }));
    const rows = accessResources(OS_REGISTRY, catalog);
    const deployables = rows.findIndex((r) => r.resource === "app:deployables");
    expect(rows[deployables]?.label).toBe("Deployables");
    const parts = rows.filter((r) => r.appId === "deployables" && r.part !== "").map((r) => r.part);
    expect(parts).toEqual(expect.arrayContaining(["deploy", "publish", "retire", "domains", "logs"]));
    // A part is `execute`; a floored section is `read`, named as the section.
    expect(rows.find((r) => r.resource === "app:deployables/publish")?.verb).toBe("execute");
    expect(rows.find((r) => r.resource === "app:settings/cluster")).toMatchObject({ verb: "read", label: "Cluster" });
    // Every app's parts come right after its door.
    for (let i = deployables + 1; i < rows.length && rows[i]!.appId === "deployables"; i++) expect(rows[i]!.part).not.toBe("");
    // A resource only a grant names is kept, so a deny on it can be lifted.
    expect(accessResources(OS_REGISTRY, catalog, ["app:ghost"]).some((r) => r.resource === "app:ghost")).toBe(true);
  });
});

describe("by person: what they can open, in words", () => {
  it("a person row says can open and where it comes from", async () => {
    const state = freshState({
      access: {
        "user:u-ada": [
          accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" }),
          accessRow({ resource: "app:cluster/origins", held: true, source: "group", sourceId: "g-acme" }),
          accessRow({ resource: "app:files", held: true, source: "person", sourceId: "u-ada" }),
          // A grant may name a verb no role opens Files with; the row is
          // about opening it, `read`, and this answer is not that.
          accessRow({ resource: "app:files", verb: "execute", held: false, source: "not-granted" }),
          accessRow({ resource: "app:deployables/publish", verb: "execute", held: false, source: "person", sourceId: "u-ada" }),
          accessRow({ resource: "app:users", held: false, source: "group", sourceId: "g-acme" }),
          accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" }),
        ],
      },
      memberships: { "u-ada": [membership("g-acme", "u-ada")] },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:deployables")).toEqual(["Can open, from the role Developer"]));
    expect(answersOf("app:cluster/origins")).toEqual(["Can open, from the group Acme"]);
    expect(answersOf("app:files")).toEqual(["Can open, granted to this person"]);
    expect(answersOf("app:deployables/publish")).toEqual(["Can't open, denied to this person"]);
    expect(answersOf("app:users")).toEqual(["Can't open, denied by the group Acme"]);
    expect(answersOf("app:cluster/audit")).toEqual(["Can't open, nothing grants it"]);
    // The engine was asked about the person, by their bare id.
    expect(state.calls.find((c) => c.name === "effectiveAccessForSubject")?.call).toContain('subjectKind: "user", subjectId: "u-ada"');
    // The scope line names the role and the groups.
    expect(screen.getByText("Ada Lovelace holds the Developer role and is in Acme.")).toBeTruthy();
  });

  it("an organization role on a cluster app says it can't open", async () => {
    const state = freshState({
      people: [ME, ADA, JANE],
      roles: [...ladderRows(), ACME_MODERATOR],
      access: {
        "user:u-jane": [
          accessRow({ resource: "app:logs", held: false, source: "scoped-role-limit", sourceId: "acme-moderator" }),
          accessRow({ resource: "app:settings/cluster", held: false, source: "scoped-role-limit", sourceId: "acme-moderator" }),
          accessRow({ resource: "app:deployables", held: true, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:email", held: true, source: "role", sourceId: "acme-moderator", accountId: "acct-7f3a" }),
          accessRow({ resource: "data", held: true, source: "role", sourceId: "acme-moderator", accountId: "acct-7f3a" }),
        ],
      },
      memberships: { "u-jane": [membership("g-acme", "u-jane")] },
    });
    mount(state);
    await pickSubject("Jane Doe");
    await waitFor(() => expect(answersOf("app:logs")).toEqual(["Can't open, an organization role can't open this"]));
    expect(answersOf("app:settings/cluster")).toEqual(["Can't open, an organization role can't open this"]);
    expect(answersOf("app:email")).toEqual(["Can open in Acme, from the role Acme moderator"]);
    // No grant lifts an organization role's limit, so nothing that would try is offered...
    expect(actsOf("app:logs")).toEqual([]);
    expect(actsOf("app:settings/cluster")).toEqual([]);
    // ...to a viewer who does write grants.
    expect(actsOf("app:deployables")).toEqual(["Deny Deployables to Jane Doe"]);
    expect(screen.getByText("Jane Doe holds the Acme moderator role and is in Acme.")).toBeTruthy();
  });

  it("a per-organization row names the organization", async () => {
    const state = freshState({
      people: [ME, ADA, MAX],
      groups: [ACME, BETA],
      // In the engine's own order: unscoped first, then by account id, which
      // puts Beta's before Acme's. The page orders organizations by name.
      access: {
        "user:u-max": [
          accessRow({ resource: "app:files", held: true, source: "role", sourceId: "writer" }),
          accessRow({ resource: "app:deployables", held: false, source: "not-granted", accountId: "acct-2b9c" }),
          accessRow({ resource: "app:email", held: true, source: "role", sourceId: "writer", accountId: "acct-2b9c" }),
          accessRow({ resource: "data", held: true, source: "role", sourceId: "writer", accountId: "acct-2b9c" }),
          accessRow({ resource: "app:deployables", held: true, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:email", held: true, source: "role", sourceId: "writer", accountId: "acct-7f3a" }),
          accessRow({ resource: "data", held: true, source: "role", sourceId: "writer", accountId: "acct-7f3a" }),
        ],
      },
      memberships: { "u-max": [membership("g-acme", "u-max"), membership("g-beta", "u-max")] },
    });
    mount(state);
    await pickSubject("Max Planck");
    await waitFor(() =>
      expect(answersOf("app:deployables")).toEqual(["Can open in Acme, from the group Acme", "Can't open in Beta, nothing grants it"]),
    );
    expect(answersOf("app:email")).toEqual(["Can open in Acme, from the role Member", "Can open in Beta, from the role Member"]);
    // A pair the engine answers once says so once, with no organization.
    expect(answersOf("app:files")).toEqual(["Can open, from the role Member"]);
    // A grant to a person reaches every organization they are in, so the
    // pair's acts are drawn once, never one per organization's answer...
    expect(actsOf("app:deployables")).toEqual(["Allow Deployables to Max Planck", "Deny Deployables to Max Planck"]);
    // ...and the page says what they reach.
    expect(screen.getByText("A grant to a person counts in every organization they are in.")).toBeTruthy();
  });

  it("a pair a global role's limit bars says which role, and offers nothing", async () => {
    // Fleet opens to the roles that manage it and no grant lifts that; the
    // engine answers it as the role's own (held false), not "not granted".
    const state = freshState({
      people: [ME, ADA, MAX],
      access: { "user:u-max": [accessRow({ resource: "app:fleet", held: false, source: "role", sourceId: "writer" })] },
    });
    mount(state);
    await pickSubject("Max Planck");
    await waitFor(() => expect(answersOf("app:fleet")).toEqual(["Can't open, the role Member can't open this"]));
    expect(actsOf("app:fleet")).toEqual([]);
  });

  it("an organization's door says can't open where its data can't be read", async () => {
    // The shell opens Email or Deployables in an organization only where the
    // person can read that organization's data too (system/roles.ts,
    // availableInAnyOrganization), so the door alone is not the answer.
    const state = freshState({
      people: [ME, ADA, MAX],
      access: {
        "user:u-max": [
          accessRow({ resource: "app:email", held: true, source: "role", sourceId: "writer", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:deployables", held: false, source: "not-granted", accountId: "acct-7f3a" }),
          accessRow({ resource: "data", held: false, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
        ],
      },
    });
    mount(state);
    await pickSubject("Max Planck");
    await waitFor(() => expect(answersOf("app:email")).toEqual(["Can't open in Acme, they can't read its data"]));
    expect(answersOf("app:deployables")).toEqual(["Can't open in Acme, nothing grants it and they can't read its data"]);
    // A grant on the door cannot change either answer, so none is offered.
    expect(actsOf("app:email")).toEqual([]);
    expect(actsOf("app:deployables")).toEqual([]);
  });

  it("reads an answer whether it crossed the wire as a boolean or as a word", async () => {
    // A scalar boolean has reached this client as the STRING "true" on a
    // builtin's reply row; read as the boolean alone, every door this person
    // can open would be drawn shut.
    const state = freshState({
      access: {
        "user:u-ada": [
          { ...accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" }), held: "true" } as Row,
          { ...accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" }), held: "false" } as Row,
        ],
      },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:deployables")).toEqual(["Can open, from the role Developer"]));
    expect(answersOf("app:cluster/audit")).toEqual(["Can't open, nothing grants it"]);
  });

  it("never shows the last person's answers while the next one's are read", async () => {
    const state = freshState({
      people: [ME, ADA, MAX],
      access: {
        "user:u-ada": [accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" })],
        "user:u-max": "pending",
      },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:deployables")).toEqual(["Can open, from the role Developer"]));
    await pickSubject("Max Planck");
    // The shape of the answer to come, and nothing of Ada's under Max's name.
    expect(await screen.findByText("Reading what they can open.")).toBeTruthy();
    expect(document.querySelector("[data-resource]")).toBeNull();
  });

  it("group rows say not granted by this group", async () => {
    const state = freshState({
      access: {
        "group:g-acme": [
          accessRow({ resource: "app:deployables", held: true, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:deployables/reviews", verb: "execute", held: true, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:logs", held: false, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
        ],
      },
    });
    mount(state);
    await pickSubject("Acme");
    await waitFor(() => expect(answersOf("app:deployables")).toEqual(["Granted"]));
    expect(answersOf("app:deployables/reviews")).toEqual(["Granted"]);
    expect(answersOf("app:logs")).toEqual(["Denied"]);
    expect(answersOf("app:files")).toEqual(["Not granted by this group"]);
    expect(state.calls.find((c) => c.name === "effectiveAccessForSubject")?.call).toContain('subjectKind: "group", subjectId: "g-acme"');
  });
});

describe("by person: the acts", () => {
  it("allow is a button in the act column", async () => {
    const state = freshState({
      people: [ME, ADA, JANE],
      roles: [...ladderRows(), ACME_MODERATOR],
      access: {
        "user:u-jane": [
          accessRow({ resource: "app:email", held: false, source: "not-granted", accountId: "acct-7f3a" }),
          accessRow({ resource: "data", held: true, source: "role", sourceId: "acme-moderator", accountId: "acct-7f3a" }),
        ],
      },
    });
    mount(state);
    await pickSubject("Jane Doe");
    await waitFor(() => expect(answersOf("app:email")).toEqual(["Can't open in Acme, nothing grants it"]));
    const row = rowFor("app:email");
    const allow = within(row).getByRole("button", { name: "Allow Email to Jane Doe" });
    // Labelled with the act, and drawn as the kit's button: the blue link that
    // read like the answer is gone.
    expect(allow.textContent).toBe("Allow");
    expect(allow.classList.contains("os-button")).toBe(true);
    expect(allow.classList.contains("os-link")).toBe(false);
    // In the row's act column, and never where the answer is read.
    expect(allow.closest(".os-record-actions")).not.toBeNull();
    for (const answer of row.querySelectorAll(".os-access-answer")) expect(answer.contains(allow)).toBe(false);
  });

  it("offers only the acts that change an answer, and Use role only over the person's own grant", async () => {
    const state = freshState({
      access: {
        "user:u-ada": [
          accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" }),
          accessRow({ resource: "app:deployables/publish", verb: "execute", held: false, source: "person", sourceId: "u-ada" }),
          accessRow({ resource: "app:files", held: true, source: "person", sourceId: "u-ada" }),
          accessRow({ resource: "app:logs", held: true, source: "group", sourceId: "g-acme" }),
          accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" }),
        ],
      },
      grantsBySubject: {
        "user:u-ada": [
          grantRow({ id: "grant-1" }),
          grantRow({ id: "grant-2", resource: "app:files", verb: "read", effect: "allow" }),
          // Revoked: the grant read still lists it, and nobody holds it.
          grantRow({ id: "grant-3", resource: "app:logs", verb: "read", effect: "deny", active: false }),
        ],
      },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(actsOf("app:deployables/publish")).toEqual(["Allow publish to Ada Lovelace", "Use the role's answer for publish"]));
    // Held from the role: only Deny changes it.
    expect(actsOf("app:deployables")).toEqual(["Deny Deployables to Ada Lovelace"]);
    // Allowed by their own grant: Deny, or back to the role; never a second Allow.
    expect(actsOf("app:files")).toEqual(["Deny Files to Ada Lovelace", "Use the role's answer for Files"]);
    // Held through a group, their own grant on it revoked: nothing of theirs to revoke.
    expect(actsOf("app:logs")).toEqual(["Deny Logs to Ada Lovelace"]);
    // Nothing grants it: only Allow changes it.
    expect(actsOf("app:cluster/audit")).toEqual(["Allow Audit trail to Ada Lovelace"]);

    // Use role revokes the very grant the person holds on that pair.
    await act(async () => {
      fireEvent.click(within(rowFor("app:deployables/publish")).getByRole("button", { name: "Use the role's answer for publish" }));
    });
    await waitFor(() => expect(state.calls.find((c) => c.name === "grantRevoke")?.call).toContain('grantId: "grant-1"'));
  });

  it("a group's row flips its own grant, or removes it", async () => {
    const state = freshState({
      access: {
        "group:g-acme": [
          accessRow({ resource: "app:deployables", held: true, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
          accessRow({ resource: "app:logs", held: false, source: "group", sourceId: "g-acme", accountId: "acct-7f3a" }),
        ],
      },
      grantsBySubject: {
        "group:g-acme": [
          grantRow({ id: "grant-g1", subjectKind: "group", subjectId: "g-acme", resource: "app:deployables", verb: "read", effect: "allow" }),
          grantRow({ id: "grant-g2", subjectKind: "group", subjectId: "g-acme", resource: "app:logs", verb: "read", effect: "deny" }),
        ],
      },
    });
    mount(state);
    await pickSubject("Acme");
    await waitFor(() => expect(actsOf("app:deployables")).toEqual(["Deny Deployables to Acme", "Remove Acme's grant for Deployables"]));
    expect(actsOf("app:logs")).toEqual(["Allow Logs to Acme", "Remove Acme's grant for Logs"]);
    expect(actsOf("app:files")).toEqual(["Allow Files to Acme"]);
  });

  it("a viewer whose role writes no grant reads the answers and is offered no act", async () => {
    const state = freshState({
      access: {
        "user:u-ada": [
          accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" }),
          accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" }),
        ],
      },
    });
    mount(state, "developer", "u-dev");
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:deployables")).toEqual(["Can open, from the role Developer"]));
    expect(answersOf("app:cluster/audit")).toEqual(["Can't open, nothing grants it"]);
    expect(screen.queryByRole("button", { name: /^(Allow|Deny|Use the role)/ })).toBeNull();
    expect(screen.getByText(/Writing a grant takes/)).toBeTruthy();
  });

  it("writes one grant, re-reads, and tells the session scope", async () => {
    const state = freshState({
      access: { "user:u-ada": [accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" })] },
    });
    const written = vi.fn();
    const off = onGrantWritten(written);
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:cluster/audit")).toEqual(["Can't open, nothing grants it"]));

    // The re-read after the write is the engine's new answer.
    state.access["user:u-ada"] = [accessRow({ resource: "app:cluster/audit", held: true, source: "person", sourceId: "u-ada" })];
    await act(async () => {
      fireEvent.click(within(rowFor("app:cluster/audit")).getByRole("button", { name: "Allow Audit trail to Ada Lovelace" }));
    });

    await waitFor(() => expect(answersOf("app:cluster/audit")).toEqual(["Can open, granted to this person"]));
    const set = state.calls.find((c) => c.name === "grantSet");
    expect(set?.call).toContain('subjectKind: "user"');
    expect(set?.call).toContain('subjectId: "u-ada"');
    expect(set?.call).toContain('verb: "read"');
    expect(set?.call).toContain('resourceType: "app:cluster/audit"');
    expect(set?.call).toContain('effect: "allow"');
    expect(state.calls.filter((c) => c.name === "effectiveAccessForSubject")).toHaveLength(2);
    expect(written).toHaveBeenCalled();
    off();
  });

  it("prints the engine's refusal beside the row that asked", async () => {
    const state = freshState({
      access: {
        "user:u-ada": [
          accessRow({ resource: "app:deployables", held: true, source: "role", sourceId: "developer" }),
          accessRow({ resource: "app:cluster/audit", held: false, source: "not-granted" }),
        ],
      },
      setReply: { ok: false, grantId: "", code: "grant_subject_outranks_caller", message: "u-ada ranks 300, above your 200" } as Row,
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:cluster/audit")).toEqual(["Can't open, nothing grants it"]));
    await act(async () => {
      fireEvent.click(within(rowFor("app:cluster/audit")).getByRole("button", { name: "Allow Audit trail to Ada Lovelace" }));
    });
    const refusal = await screen.findByText("They rank above you");
    // Beside the row: inside the Audit trail row, and nowhere else.
    expect(rowFor("app:cluster/audit").contains(refusal)).toBe(true);
    expect(rowFor("app:deployables").contains(refusal)).toBe(false);
    expect(screen.getByText(/u-ada ranks 300/)).toBeTruthy();
  });

  it("does not offer to grant to yourself", async () => {
    mount(freshState({ access: { "user:u-me": [accessRow({ resource: "app:users", held: true, source: "role", sourceId: "owner" })] } }));
    await pickSubject("Owner");
    await waitFor(() => expect(answersOf("app:users")).toEqual(["Can open, from the role Owner"]));
    expect(screen.queryByRole("button", { name: /^(Allow|Deny|Use the role)/ })).toBeNull();
    expect(screen.getByText(/Nobody grants to themselves/)).toBeTruthy();
  });
});

describe("by person: a read the engine refuses", () => {
  it("an organization's own admin gets a notice in place of the answers", async () => {
    // An organization role at admin rank may open Access, and the engine
    // refuses it every person's answers: that read is a cluster operator's.
    const state = freshState({
      roles: [...ladderRows(), ACME_ADMIN],
      access: {
        "user:u-ada": {
          refusal: 'access_caller_not_permitted: reading somebody\'s access needs a cluster role at admin rank or above; yours is "acme-admin"',
        },
      },
      grantReadsRefused: true,
    });
    mount(state, "acme-admin", "u-boss");
    await pickSubject("Ada Lovelace");
    expect(await screen.findByText("Your role cannot read what other people can open")).toBeTruthy();
    expect(screen.getByText(/needs a cluster role at admin rank or above/)).toBeTruthy();
    // No answers, no acts, nothing else about them (the reads behind the scope
    // line are refused too), and no second notice for those reads.
    expect(document.querySelector("[data-resource]")).toBeNull();
    expect(screen.queryByRole("button", { name: /^(Allow|Deny|Use the role)/ })).toBeNull();
    expect(screen.queryByText(/holds the .* role/)).toBeNull();
    expect(screen.queryByText("Their grants could not be read.")).toBeNull();
  });

  it("a developer reading the owner sees no answers", async () => {
    const state = freshState({
      access: {
        "user:u-me": { refusal: "access_subject_outranks_caller: that person ranks above you, and nobody reads the access of somebody who outranks them" },
      },
    });
    mount(state, "developer", "u-dev");
    await pickSubject("Owner");
    expect(await screen.findByText("They rank above you")).toBeTruthy();
    expect(document.querySelector("[data-resource]")).toBeNull();
  });
});

describe("by app: who holds it", () => {
  it("lists the roles that hold it and everyone granted it by name", async () => {
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-1", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state);
    fireEvent.click(screen.getByRole("button", { name: "By app" }));
    const trigger = await screen.findByLabelText("App or part");
    await waitFor(() => chooseOption(trigger, "Cluster: Data"));
    await waitFor(() => expect(screen.getByText("By role: Owner.")).toBeTruthy());
    const list = screen.getByRole("list", { name: "Granted by name" });
    expect(within(list).getByText("Ada Lovelace")).toBeTruthy();
    expect(within(list).getByText("allowed")).toBeTruthy();
    expect(within(list).getByRole("button", { name: "Revoke allow for Ada Lovelace" })).toBeTruthy();
  });

  it("never prints a principal id where a name goes", async () => {
    // A grant naming somebody the roster does not carry -- a person below the
    // roster's floor, or a row since deactivated. An id names nobody a reader
    // can look up, and this list is the one place it could leak.
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-x", subjectId: "u-ghost", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state);
    const list = await pickResource("Cluster: Data");
    expect(within(list).getByText("Not on this roster")).toBeTruthy();
    expect(list.textContent).not.toContain("u-ghost");
    // The act's label says the same thing, for the same reason.
    expect(within(list).getByRole("button", { name: "Revoke allow for Not on this roster" })).toBeTruthy();
  });

  it("offers no Revoke on the viewer's own grant, and says why", async () => {
    // The engine's rule 4 refuses a grant naming yourself, revoke included
    // (`checkGrantAuthority`), so the act would only ever be refused.
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-me", subjectId: "u-me", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state);
    const list = await pickResource("Cluster: Data");
    expect(within(list).getByText("you")).toBeTruthy();
    expect(within(list).queryByRole("button", { name: /^Revoke/ })).toBeNull();
    expect(screen.getByText(/Nobody revokes their own grant/)).toBeTruthy();
  });

  it("still offers Revoke on a group the viewer belongs to", async () => {
    // Rule 4 is about a USER subject. A group is somebody else's to hold
    // even when the viewer is in it, so the act stays.
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-g", subjectKind: "group", subjectId: "g-acme", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state);
    const list = await pickResource("Cluster: Data");
    expect(within(list).getByRole("button", { name: "Revoke allow for Acme" })).toBeTruthy();
    expect(screen.queryByText(/Nobody revokes their own grant/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The viewer's id SPELLING
// ---------------------------------------------------------------------------
// THE REGRESSION. Every case above gives the session a bare `u-me`, which is
// the one shape that cannot catch this: a deployed cluster's token carries
// the canonical `v1:identity:user:...` while grant rows and the roster are
// bare, and compared raw the viewer matches nobody -- including themselves.
// Each failure is quiet and points the wrong way: the screen offers Allow and
// Deny on your own row (the engine then refuses `grant_self`, so the act is
// advertised and cannot be performed), and "by you" degrades into your own
// display name as though a colleague had written the grant.

const CANONICAL_ME = "v1:identity:user:u-me";

describe("a canonical viewer id against bare subjects", () => {
  it("knows the viewer is themselves, and offers no act on their own row", async () => {
    mount(freshState({ access: { "user:u-me": [accessRow({ resource: "app:users", held: true, source: "role", sourceId: "owner" })] } }), "owner", CANONICAL_ME);
    await pickSubject("Owner");
    await waitFor(() => expect(answersOf("app:users")).toEqual(["Can open, from the role Owner"]));
    expect(screen.queryByRole("button", { name: /^(Allow|Deny)/ })).toBeNull();
    expect(screen.getByText(/Nobody grants to themselves/)).toBeTruthy();
  });

  it("still says 'by you' for a grant the viewer wrote", async () => {
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-1", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state, "owner", CANONICAL_ME);
    const list = await pickResource("Cluster: Data");
    expect(within(list).getByText("a person, by you")).toBeTruthy();
    // ...and not the viewer's own display name, which is what the raw
    // comparison falls through to: the roster names them, so the provenance
    // reads as a colleague's decision rather than as the reader's own.
    expect(list.textContent).not.toContain("by Owner");
  });

  it("keeps the viewer's own grant out of the by-app view's acts", async () => {
    const state = freshState({
      grantsByResource: {
        "app:cluster/origins": [grantRow({ id: "grant-me", subjectId: "u-me", resource: "app:cluster/origins", verb: "read", effect: "allow" })],
      },
    });
    mount(state, "owner", CANONICAL_ME);
    const list = await pickResource("Cluster: Data");
    expect(within(list).getByText("you")).toBeTruthy();
    expect(within(list).queryByRole("button", { name: /^Revoke/ })).toBeNull();
  });
});

describe("a canonical roster id against a bare session", () => {
  it("still resolves the person the picker names, id colons and all", async () => {
    // The mirror case: the ROSTER carries canonical ids. The picker's value
    // is `<kind>:<id>`, so a canonical id puts three more colons into it --
    // cut at the second, the key resolves to nobody and choosing a person
    // selects nothing at all.
    const state = freshState({
      people: [ME, { ...ADA, id: "v1:identity:user:u-ada" } as Row],
      access: {
        "user:u-ada": [accessRow({ resource: "app:deployables/publish", verb: "execute", held: false, source: "person", sourceId: "u-ada" })],
      },
      grantsBySubject: { "user:u-ada": [grantRow({ id: "grant-1" })] },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(answersOf("app:deployables/publish")).toEqual(["Can't open, denied to this person"]));
    // The subject reached the wire BARE, which is the spelling grant rows
    // store and the engine reads: `grantsForSubject` filters on the payload
    // field, so a canonical argument matches no row and "Use role" would have
    // no grant to revoke.
    expect(state.calls.find((c) => c.name === "effectiveAccessForSubject")?.call).toContain('subjectId: "u-ada"');
    expect(state.calls.find((c) => c.name === "grantsForSubject")?.call).toContain('subjectId: "u-ada"');
    expect(actsOf("app:deployables/publish")).toEqual(["Allow publish to Ada Lovelace", "Use the role's answer for publish"]);
  });
});

describe("refusal copy coverage", () => {
  it("has copy for every governance code the grant builtins emit", () => {
    const grantsGo = join(dirname(fileURLToPath(import.meta.url)), "../../../../integrations/rbac/grants.go");
    const source = readFileSync(grantsGo, "utf8");
    const codes = [...source.matchAll(/codeGrant\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]!).sort();
    expect(codes.length).toBeGreaterThan(4);
    expect(knownGrantCodes()).toEqual(codes);
  });
});
