import { act, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { accountRow, fakeConnection, grantRow, roleRow, userRow, withSession } = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");
const { ROLE_GRID_VOCABULARY, cellState, gridPairs } = await import("../../src/apps/users/grid");
const { placementSentence, proposeRank, slugFrom } = await import("../../src/apps/users/NewRolePage");

type Conn = ReturnType<typeof fakeConnection>;

const LADDER = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
  roleRow({ slug: "acme-lead", name: "Acme lead", rank: 120, predefined: false }),
];

const GRANTS = [
  grantRow("owner", "read", "principal"),
  grantRow("owner", "create", "principal"),
  grantRow("owner", "update", "principal"),
  grantRow("owner", "delete", "principal"),
  grantRow("owner", "read", "role"),
  grantRow("owner", "create", "role"),
  grantRow("owner", "update", "role"),
  grantRow("owner", "delete", "role"),
  grantRow("owner", "read", "data"),
  grantRow("admin", "read", "principal"),
  grantRow("admin", "read", "role"),
  grantRow("acme-lead", "read", "data"),
];

function seed(extra: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [accountRow({ id: "acct-acme", name: "Acme" })],
    ...extra,
  });
}

function mount(connection: Conn, role = "owner") {
  h.connection = connection;
  return renderApp(
    withSession(
      <UsersApp sectionId="roles" navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
      { role },
    ),
  );
}

// ===========================================================================
// THE LADDER
// ===========================================================================

describe("the roles list", () => {
  it("draws the ladder rank descending, with the holders each has", async () => {
    const view = mount(
      seed({
        searchUsers: [
          userRow({ id: "u1", displayName: "Ada", role: "admin" }),
          userRow({ id: "u2", displayName: "Kit", role: "writer" }),
        ],
      }),
    );
    const list = await screen.findByRole("list", { name: /This cluster's roles/ });
    const names = within(list)
      .getAllByRole("button")
      .map((node) => node.textContent ?? "");
    expect(names[0]).toContain("Owner");
    expect(names[1]).toContain("Developer");
    expect(names[2]).toContain("Admin");
    // `writer` is an ALIAS of `user`, and the count has to resolve it or every
    // ordinary principal reads as holding nothing.
    expect(names.find((n) => n.includes("Member"))).toContain("1 person");
    expect(names.find((n) => n.includes("Admin"))).toContain("1 person");
    view.unmount();
  });

  it("offers New role only to somebody who holds create on role", async () => {
    const owner = mount(seed());
    expect(await screen.findByRole("button", { name: "New role" })).toBeTruthy();
    owner.unmount();

    const admin = mount(seed(), "admin");
    await screen.findByRole("list", { name: /This cluster's roles/ });
    expect(screen.queryByRole("button", { name: "New role" })).toBeNull();
    admin.unmount();
  });
});

// ===========================================================================
// THE GRID
// ===========================================================================

describe("the grid", () => {
  it("names a pair nothing gates as absent rather than as an unchecked box", () => {
    // An unchecked box says "off", which is an answer. A dash says the
    // question is not asked -- and a checkbox in its place would write a grant
    // no resolver ever reads.
    expect(gridPairs()).toContain("principal:read");
    expect(gridPairs()).not.toContain("principal:execute");
    expect(
      cellState({ resource: "principal", verb: "execute", held: false, callerHolds: true, editable: true }),
    ).toBe("absent");
  });

  it("locks a permission the role holds and the caller does not", () => {
    // Hiding it would misreport what the role holds; offering it would let
    // somebody hand on an authority they do not have, which is the guard
    // roleUpdate applies server-side.
    expect(
      cellState({ resource: "principal", verb: "delete", held: true, callerHolds: false, editable: true }),
    ).toBe("lockedOn");
  });

  it("locks a predefined role's whole grid, in both values", () => {
    expect(
      cellState({ resource: "data", verb: "read", held: true, callerHolds: true, editable: false }),
    ).toBe("lockedOn");
    expect(
      cellState({ resource: "data", verb: "delete", held: false, callerHolds: true, editable: false }),
    ).toBe("lockedOff");
  });

  it("is one line, so the Go parity gate can read it", () => {
    // component/memql/role_grid_os_parity_test.go matches a one-line literal
    // and fails on anything computed: it compares NAMES, and a value assembled
    // at runtime is one it cannot read.
    expect(ROLE_GRID_VOCABULARY.includes("\n")).toBe(false);
    expect(ROLE_GRID_VOCABULARY).toContain("principal:read,create,update,delete");
  });

  it("draws a predefined role read-only and says why", async () => {
    const view = mount(seed());
    await click(await screen.findByRole("button", { name: /Admin/ }));
    await screen.findByText("Permissions");
    expect(screen.getByText(/A predefined role is the cluster's own/)).toBeTruthy();
    const cell = screen.getByRole("checkbox", { name: "Read People" });
    expect((cell as HTMLButtonElement).disabled).toBe(true);
    view.unmount();
  });
});

// ===========================================================================
// A ROLE'S PAGE
// ===========================================================================

describe("a role's page", () => {
  it("keeps Deactivate off a role somebody holds, and says how many", async () => {
    // A role somebody holds cannot be retired without deciding what those
    // people become, and that decision is not this button's to make (rule 12).
    const view = mount(
      seed({ searchUsers: [userRow({ id: "u1", displayName: "Ada", role: "acme-lead" })] }),
    );
    await click(await screen.findByRole("button", { name: /Acme lead/ }));
    await screen.findByText("Permissions");
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button", { name: "Deactivate" })).toBeNull();
    expect(screen.getByText(/Held by 1 person; move them first/)).toBeTruthy();
    view.unmount();
  });

  it("offers Deactivate on a custom role nobody holds", async () => {
    const view = mount(seed({ searchUsers: [] }));
    await click(await screen.findByRole("button", { name: /Acme lead/ }));
    await screen.findByText("Permissions");
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByRole("button", { name: "Deactivate" })).toBeTruthy();
    view.unmount();
  });

  it("reads a refusal out of the DECISION ROW rather than reporting success", async () => {
    // `integrations/rbac` does not refuse with an error the way
    // `integrations/groups` does: its handlers answer `{ok, slug, code,
    // message}` as an ordinary reply row, so the call SUCCEEDS and the refusal
    // rides inside it. Read naively, every refused role write reports success
    // -- the form closes, nothing is written, and the person is told nothing.
    const connection = seed({
      searchUsers: [],
      builtins: {
        roleUpdate: {
          ok: false,
          slug: "acme-lead",
          code: "role_grant_not_held",
          message: "you do not hold delete on principal",
        },
      },
    });
    const view = mount(connection);
    await click(await screen.findByRole("button", { name: /Acme lead/ }));
    await screen.findByText("Permissions");

    await click(screen.getByRole("checkbox", { name: "Delete People" }));
    await click(screen.getByRole("button", { name: "Save permissions" }));

    // The code's own copy, and the server's sentence verbatim beneath it.
    expect(
      await screen.findByText(/You do not hold a permission you are trying to grant/),
    ).toBeTruthy();
    expect(screen.getByText(/you do not hold delete on principal/)).toBeTruthy();
    // AND THE EDIT IS STILL ON SCREEN: a refused save must not read as a
    // saved one, so the draft stays and Save is still offered.
    expect(screen.getByRole("button", { name: "Save permissions" })).toBeTruthy();
    view.unmount();
  });

  it("keeps an unknown decision code's own sentence under a neutral heading", async () => {
    const connection = seed({
      searchUsers: [],
      builtins: {
        roleUpdate: { ok: false, code: "role_something_new", message: "the cluster said this" },
      },
    });
    const view = mount(connection);
    await click(await screen.findByRole("button", { name: /Acme lead/ }));
    await screen.findByText("Permissions");
    await click(screen.getByRole("checkbox", { name: "Delete People" }));
    await click(screen.getByRole("button", { name: "Save permissions" }));

    expect(await screen.findByText(/the cluster said this/)).toBeTruthy();
    view.unmount();
  });
});

// ===========================================================================
// THE NEW ROLE RAIL
// ===========================================================================

describe("proposing a slot on the ladder", () => {
  const ladder = LADDER.map((row) => ({
    id: String(row["id"]),
    slug: String(row["slug"]),
    name: String(row["name"]),
    rank: Number(row["rank"]),
    description: "",
    predefined: Boolean(row["predefined"]),
    active: true,
    aliases: [] as string[],
    accountId: "",
  }));

  it("derives a slug from the display name", () => {
    expect(slugFrom("Field Engineer")).toBe("field-engineer");
    expect(slugFrom("  Acme  lead ")).toBe("acme-lead");
  });

  it("proposes the base's rank plus 20", () => {
    // The catalog's ranks are spaced 100 apart precisely so a custom role slots
    // between two of them.
    const base = ladder.find((r) => r.slug === "admin")!;
    expect(proposeRank(base, ladder, 400)).toBe(220);
  });

  it("walks up to the next free slot when the proposal is taken", () => {
    // acme-lead already sits at 120, which is exactly user's 100 plus 20 -- and
    // two rungs at one rank have no order between them, which is why the
    // engine refuses it and why proposing one would propose a refusal.
    const base = ladder.find((r) => r.slug === "user")!;
    expect(proposeRank(base, ladder, 400)).toBe(121);
  });

  it("says where the rung lands, in the ladder's own words", () => {
    expect(placementSentence(210, ladder)).toContain("just above Admin");
  });

  it("says a rank at developer and above is staff", async () => {
    const view = mount(seed());
    await click(await screen.findByRole("button", { name: "New role" }));
    await screen.findByRole("heading", { name: "New role" });
    const { fireEvent } = await import("@testing-library/react");
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Field engineer" } });
    });
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Rank"), { target: { value: "320" } });
    });
    expect(
      await screen.findByText(/A role at this rank is staff: in every account's group, standing/),
    ).toBeTruthy();
    view.unmount();
  });

  it("refuses a taken rank and a rank at or above the caller's, in words", async () => {
    const view = mount(seed(), "admin");
    // An admin holds no create-on-role here, so the rail is not offered at all
    // -- which is the honest answer, and the case the list test pins.
    expect(screen.queryByRole("button", { name: "New role" })).toBeNull();
    view.unmount();

    const owner = mount(seed());
    await click(await screen.findByRole("button", { name: "New role" }));
    const { fireEvent } = await import("@testing-library/react");
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Taken" } });
    });
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Rank"), { target: { value: "200" } });
    });
    expect(await screen.findByText(/Rank 200 is taken/)).toBeTruthy();

    await act(async () => {
      fireEvent.change(screen.getByLabelText("Rank"), { target: { value: "400" } });
    });
    expect(await screen.findByText(/at or above your own/)).toBeTruthy();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button", { name: "Create role" })).toBeNull();
    owner.unmount();
  });
});
