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
import { accessResources, knownGrantCodes, resolveAnswer, type AccessGrant, type Subject } from "../../src/apps/settings/access/model";
import { OS_REGISTRY } from "../../src/apps/registry";
import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { onGrantWritten } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { installSeededAccess, SEEDED_CAPABILITIES } from "../seededAccess";
import { chooseOption } from "../selectControl";
import { SEEDED_LADDER } from "../seededLadder";

// SETTINGS > ACCESS (epic memql#5289, task memql#5307): the by-person and
// by-app views over the two grant builtins. The acceptance the task names:
// a denied part shows "denied, by you"; an inherited one shows the role; a
// developer viewing the owner sees the matrix read-only; every governance
// refusal code has copy.

const ME: Row = { id: "u-me", displayName: "Owner", primaryEmail: "owner@example.com", role: "owner", active: true } as Row;
const ADA: Row = { id: "u-ada", displayName: "Ada Lovelace", primaryEmail: "ada@example.com", role: "developer", active: true } as Row;
const ACME: Row = { id: "g-acme", name: "Acme", description: "", kind: "account", accountId: "acct-acme", status: "active", createdAt: "2026-09-01T00:00:00Z" } as Row;

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

interface State {
  grantsBySubject: Record<string, Row[]>;
  grantsByResource: Record<string, Row[]>;
  memberships: Record<string, Row[]>;
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
          return rows([ME, ADA]);
        case "groupsAll":
          return rows([ACME]);
        case "activeRoles":
          return rows(ladderRows());
        case "activeCapabilities":
          return rows(catalogRows());
        case "grantsForSubject":
          return rows(state.grantsBySubject[`${argOf(call, "subjectKind")}:${argOf(call, "subjectId")}`] ?? []);
        case "groupsForUser":
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
    grantsBySubject: {},
    grantsByResource: {},
    memberships: {},
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

function mount(state: State, role = "owner") {
  installSeededAccess(role);
  h.connection = fakeConnection(state);
  return render(
    <SessionProvider
      value={{
        access: { userId: "u-me", primaryEmail: "owner@example.com", role, roleName: "", rank: 0 },
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

function rowFor(resource: string): HTMLElement {
  const row = document.querySelector(`tr[data-resource="${resource}"]`);
  if (row === null) throw new Error(`no row for ${resource}`);
  return row as HTMLElement;
}

beforeEach(() => {
  h.connection = null;
});

afterEach(() => {
  installSeededAccess("owner");
});

describe("the pure half", () => {
  it("resolves most specific wins across levels, deny wins within one", () => {
    const catalog = catalogRows().map((r) => ({
      id: String(r.id),
      roleSlug: String(r.roleSlug),
      verb: String(r.verb),
      resourceType: String(r.resourceType),
      effect: r.effect === "deny" ? ("deny" as const) : ("allow" as const),
      predefined: true,
      active: true,
    }));
    const ada: Subject = { kind: "user", id: "u-ada", name: "Ada", role: "developer" };
    const allow = (over: Partial<AccessGrant>): AccessGrant => ({
      id: "g",
      subjectKind: "user",
      subjectId: "u-ada",
      verb: "execute",
      resource: "app:deployables/publish",
      effect: "allow",
      grantedBy: "u-me",
      active: true,
      createdAt: "",
      ...over,
    });
    // The role holds it.
    expect(resolveAnswer(ada, "execute", "app:deployables/publish", catalog, [], [])).toMatchObject({ held: true, source: "role" });
    // A group deny beats the role.
    expect(resolveAnswer(ada, "execute", "app:deployables/publish", catalog, [allow({ effect: "deny", subjectKind: "group", subjectId: "g-acme" })], [])).toMatchObject({ held: false, source: "group" });
    // A user allow beats the group deny.
    expect(
      resolveAnswer(ada, "execute", "app:deployables/publish", catalog, [allow({ effect: "deny", subjectKind: "group", subjectId: "g-acme" })], [allow({})]),
    ).toMatchObject({ held: true, source: "user" });
    // Two groups disagree: deny wins within the level.
    expect(
      resolveAnswer(ada, "execute", "app:deployables/publish", catalog, [allow({ subjectKind: "group", subjectId: "g-1" }), allow({ effect: "deny", subjectKind: "group", subjectId: "g-2" })], []),
    ).toMatchObject({ held: false, source: "group" });
    // An inactive grant is ignored.
    expect(resolveAnswer(ada, "execute", "app:deployables/publish", catalog, [], [allow({ effect: "deny", active: false })])).toMatchObject({ held: true, source: "role" });
    // A group subject has no role level.
    const acme: Subject = { kind: "group", id: "g-acme", name: "Acme", role: "" };
    expect(resolveAnswer(acme, "read", "app:users", catalog, [], [])).toMatchObject({ held: false, source: "role" });
  });

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
    expect(parts).toEqual(expect.arrayContaining(["sources", "deploy", "publish", "retire", "domains", "logs"]));
    // A part is `execute`; a floored section is `read`, named as the section.
    expect(rows.find((r) => r.resource === "app:deployables/publish")?.verb).toBe("execute");
    expect(rows.find((r) => r.resource === "app:settings/cluster")).toMatchObject({ verb: "read", label: "Cluster" });
    // Every app's parts come right after its door.
    for (let i = deployables + 1; i < rows.length && rows[i]!.appId === "deployables"; i++) expect(rows[i]!.part).not.toBe("");
    // A resource only a grant names is kept, so a deny on it can be lifted.
    expect(accessResources(OS_REGISTRY, catalog, ["app:ghost"]).some((r) => r.resource === "app:ghost")).toBe(true);
  });
});

describe("by person: the matrix", () => {
  it("a denied part shows 'denied, by you'; an inherited one shows the role", async () => {
    const state = freshState({
      grantsBySubject: { "user:u-ada": [grantRow({ id: "grant-1" })] },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => {
      expect(rowFor("app:deployables/publish").textContent).toContain("denied, by you");
    });
    expect(rowFor("app:deployables").textContent).toContain("developer role");
    expect(rowFor("app:deployables").querySelector('[data-state="on"]')).not.toBeNull();
    // A resource the role lacks says so, from the role.
    expect(rowFor("app:stores").querySelector('[data-state="off"]')).not.toBeNull();
    // The scope line names the role and the groups.
    expect(screen.getByText(/Ada Lovelace holds the Developer role and is in no group\./)).toBeTruthy();
  });

  it("names the group an answer came through", async () => {
    const state = freshState({
      grantsBySubject: { "group:g-acme": [grantRow({ id: "grant-g", subjectKind: "group", subjectId: "g-acme", resource: "app:stores", verb: "read", effect: "allow" })] },
      memberships: { "u-ada": [{ id: "m-1", groupId: "g-acme", userId: "u-ada", status: "active", origin: "added", createdAt: "" } as Row] },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => {
      expect(rowFor("app:stores").textContent).toContain("allowed, through Acme by you");
    });
    expect(screen.getByText(/is in Acme\./)).toBeTruthy();
  });

  it("offers only the act that changes the answer, and Use role beside a grant", async () => {
    const state = freshState({
      grantsBySubject: { "user:u-ada": [grantRow({ id: "grant-1" })] },
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(rowFor("app:deployables/publish").textContent).toContain("denied"));
    // Held from the role: Deny only.
    const door = rowFor("app:deployables");
    expect(within(door).getByRole("button", { name: "Deny Deployables to Ada Lovelace" })).toBeTruthy();
    expect(within(door).queryByRole("button", { name: /^Allow/ })).toBeNull();
    // Denied by a grant: Use role, and Allow.
    const publish = rowFor("app:deployables/publish");
    expect(within(publish).getByRole("button", { name: "Use the role's answer for publish" })).toBeTruthy();
    expect(within(publish).getByRole("button", { name: "Allow publish to Ada Lovelace" })).toBeTruthy();
    expect(within(publish).queryByRole("button", { name: /^Deny/ })).toBeNull();
  });

  it("a developer viewing the owner sees the matrix read-only", async () => {
    const state = freshState();
    mount(state, "developer");
    await pickSubject("Owner");
    await waitFor(() => expect(rowFor("app:users").textContent).toContain("owner role"));
    expect(screen.queryByRole("button", { name: /^(Allow|Deny|Use the role)/ })).toBeNull();
    expect(screen.getByText(/Writing a grant takes/)).toBeTruthy();
  });

  it("writes one grant, re-reads, and tells the session scope", async () => {
    const state = freshState();
    const written = vi.fn();
    const off = onGrantWritten(written);
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(rowFor("app:stores").querySelector('[data-state="off"]')).not.toBeNull());

    // The re-read after the write answers the new grant.
    state.grantsBySubject["user:u-ada"] = [grantRow({ id: "grant-new", resource: "app:stores", verb: "read", effect: "allow" })];
    await act(async () => {
      fireEvent.click(within(rowFor("app:stores")).getByRole("button", { name: "Allow Stores to Ada Lovelace" }));
    });

    await waitFor(() => expect(rowFor("app:stores").textContent).toContain("allowed, by you"));
    const set = state.calls.find((c) => c.name === "grantSet");
    expect(set?.call).toContain('subjectKind: "user"');
    expect(set?.call).toContain('subjectId: "u-ada"');
    expect(set?.call).toContain('verb: "read"');
    expect(set?.call).toContain('resourceType: "app:stores"');
    expect(set?.call).toContain('effect: "allow"');
    expect(written).toHaveBeenCalled();
    off();
  });

  it("prints the engine's refusal beside the row that asked", async () => {
    const state = freshState({
      setReply: { ok: false, grantId: "", code: "grant_subject_outranks_caller", message: "u-ada ranks 300, above your 200" } as Row,
    });
    mount(state);
    await pickSubject("Ada Lovelace");
    await waitFor(() => expect(rowFor("app:stores").querySelector('[data-state="off"]')).not.toBeNull());
    await act(async () => {
      fireEvent.click(within(rowFor("app:stores")).getByRole("button", { name: "Allow Stores to Ada Lovelace" }));
    });
    const refusal = await screen.findByText("They rank above you");
    // Beside the row: the notice's row follows the Stores row.
    expect(refusal.closest("tr")?.previousElementSibling).toBe(rowFor("app:stores"));
    expect(screen.getByText(/u-ada ranks 300/)).toBeTruthy();
  });

  it("does not offer to grant to yourself", async () => {
    mount(freshState());
    await pickSubject("Owner");
    await waitFor(() => expect(rowFor("app:users").textContent).toContain("owner role"));
    expect(screen.queryByRole("button", { name: /^(Allow|Deny)/ })).toBeNull();
    expect(screen.getByText(/Nobody grants to themselves/)).toBeTruthy();
  });
});

describe("by app: who holds it", () => {
  it("lists the roles that hold it and everyone granted it by name", async () => {
    const state = freshState({
      grantsByResource: {
        "app:stores": [grantRow({ id: "grant-1", resource: "app:stores", verb: "read", effect: "allow" })],
      },
    });
    mount(state);
    fireEvent.click(screen.getByRole("radio", { name: "By app" }));
    const trigger = await screen.findByLabelText("App or part");
    await waitFor(() => chooseOption(trigger, "Stores"));
    await waitFor(() => expect(screen.getByText("By role: Owner.")).toBeTruthy());
    const list = screen.getByRole("list", { name: "Granted by name" });
    expect(within(list).getByText("Ada Lovelace")).toBeTruthy();
    expect(within(list).getByText("allowed")).toBeTruthy();
    expect(within(list).getByRole("button", { name: "Revoke allow for Ada Lovelace" })).toBeTruthy();
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
