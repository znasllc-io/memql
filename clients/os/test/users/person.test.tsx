import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { installSeededAccess } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const {
  accountRow,
  adminOk,
  adminRefusal,
  fakeConnection,
  grantRow,
  groupRow,
  membershipRow,
  roleRow,
  userRow,
  withSession,
} = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");

type Conn = ReturnType<typeof fakeConnection>;

const LADDER = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
  roleRow({ slug: "acme-lead", name: "Acme lead", rank: 120, predefined: false, accountId: "acct-acme" }),
];

const GRANTS = [
  // owner: everything on principal, and the role verbs.
  grantRow("owner", "read", "principal"),
  grantRow("owner", "create", "principal"),
  grantRow("owner", "update", "principal"),
  grantRow("owner", "delete", "principal"),
  grantRow("owner", "update", "group"),
  grantRow("owner", "create", "admission"),
  // admin: people authority, minus delete.
  grantRow("admin", "read", "principal"),
  grantRow("admin", "create", "principal"),
  grantRow("admin", "update", "principal"),
  grantRow("admin", "update", "group"),
  grantRow("admin", "create", "admission"),
  // developer: reads the roster and invites, and manages nobody (memql#4917).
  grantRow("developer", "read", "principal"),
  grantRow("developer", "create", "admission"),
];

const ADA = userRow({
  id: "v1:identity:user:ada",
  displayName: "Ada",
  primaryEmail: "ada@example.com",
  role: "user",
});

function seed(extra: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    searchUsers: [ADA],
    byId: { "v1:identity:user:ada": ADA },
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [accountRow({ id: "acct-acme", name: "Acme", domain: "acme.com" })],
    groupsAll: [groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme" })],
    ...extra,
  });
}

async function openAda(connection: Conn, role = "owner", showDeactivated = false) {
  installSeededAccess(role);
  h.connection = connection;
  const view = renderApp(
    withSession(
      <UsersApp
        sectionId="people"
        navigate={() => {}}
        askContext={() => {}}
        store={memoryStore({ showDeactivated })}
      />,
      { role },
    ),
  );
  await click(await screen.findByRole("button", { name: /Ada/ }));
  await screen.findByText("Role");
  return view;
}

/** The rungs the ladder is offering, by their accessible state. */
function offeredRungs(): string[] {
  const ladder = screen.getByRole("list", { name: /The cluster's roles/ });
  return within(ladder)
    .getAllByRole("button")
    .filter((node) => !(node as HTMLButtonElement).disabled)
    .map((node) => node.textContent ?? "");
}

// ===========================================================================
// THE OFFERED-RUNG RULE, MIRRORED FROM auth.MayAssignRole
// ===========================================================================
// The server is the authority and refuses the same write whatever this
// renders. What these cases pin is that the ladder OFFERS what the server
// would accept: a ladder offering every rung teaches an operator they may hand
// out any of them, and they find out otherwise by being refused.

describe("the offered rungs", () => {
  it("offers an owner every rung below their own, and their own", async () => {
    await openAda(seed());
    const offered = offeredRungs().join(" ");
    // The owner carve-out: two owners share a rank, so a strict comparison
    // cannot express owner -> owner, and a cluster could never name a second
    // owner or hand itself on.
    expect(offered).toContain("Owner");
    expect(offered).toContain("Admin");
    expect(offered).toContain("Viewer");
  });

  it("offers an admin nothing at or above their own rung", async () => {
    await openAda(seed(), "admin");
    const offered = offeredRungs().join(" ");
    expect(offered).toContain("Viewer");
    // developer is 300, admin 200 -- the ladder's own ordering, which the
    // shell reads from the cluster rather than carrying its own copy of.
    expect(offered).not.toContain("Developer");
    expect(offered).not.toContain("Owner");
    expect(screen.getByText(/Only rungs below your own are offered/)).toBeTruthy();
  });

  it("offers a developer no rung at all, and says why", async () => {
    // Developer ranks ABOVE admin and holds strictly fewer principal verbs:
    // read-on-principal and create-on-admission, and no update. Rank alone
    // would offer them four rungs the server refuses one by one.
    await openAda(seed(), "developer");
    expect(offeredRungs()).toEqual([]);
    expect(screen.getByText(/does not carry changing what somebody is/)).toBeTruthy();
  });

  it("does not offer a scoped role to somebody outside that client's group", async () => {
    // The rung is DRAWN and dashed rather than dropped: a ladder that showed a
    // different number of rungs on two people's pages would leave the reader
    // with nothing to explain the difference.
    const view = await openAda(seed());
    const ladder = screen.getByRole("list", { name: /The cluster's roles/ });
    expect(within(ladder).getByText("Acme lead")).toBeTruthy();
    expect(offeredRungs().join(" ")).not.toContain("Acme lead");
    view.unmount();
  });

  it("offers a scoped role once the person is in that client's group", async () => {
    const view = await openAda(
      seed({
        groupsForUser: {
          "v1:identity:user:ada": [
            membershipRow({ id: "m1", groupId: "g-acme", userId: "v1:identity:user:ada" }),
          ],
        },
      }),
    );
    await waitFor(() => expect(offeredRungs().join(" ")).toContain("Acme lead"));
    view.unmount();
  });
});

// ===========================================================================
// THE WRITES
// ===========================================================================

describe("opening a person", () => {
  it("does not ask for sessions or offer management without the required capabilities", async () => {
    const connection = seed();
    await openAda(connection, "developer");
    expect(connection.query.sessionsForSubjectAdmin).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Deactivate" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add to a group" })).toBeNull();
    expect(screen.getByText("Signs in with")).toBeTruthy();
  });

  it.each(["admin", "owner"])("%s can read sessions and manage lower-ranked people", async role => {
    const connection = seed();
    await openAda(connection, role);
    await waitFor(() => expect(connection.query.sessionsForSubjectAdmin).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "Deactivate" })).toBeTruthy();
  });

  it("re-reads them through the authorized path rather than trusting the list", async () => {
    // `v1:identity:user` broadcasts CREATES ONLY -- the row churns on
    // `lastSeenAt`, so an update rule would be an event per person per
    // heartbeat across the mesh forever. The list's copy is therefore only as
    // fresh as the seed, which makes a page opened deliberately about one row
    // exactly the right place to pay for one authorized read.
    const connection = seed();
    const view = await openAda(connection);
    const rereads = connection.query.executeNamed.mock.calls.filter((c: unknown[]) =>
      String(c[1] ?? "").includes("v1:identity:user:ada"),
    );
    expect(rereads.length).toBeGreaterThan(0);
    view.unmount();
  });
});

describe("changing a role", () => {
  it("shows the accepted value, and only after the server accepted it", async () => {
    const connection = seed();
    connection.dispatcher.sendAndWait.mockResolvedValue(adminOk());
    await openAda(connection);

    const ladder = screen.getByRole("list", { name: /The cluster's roles/ });
    await click(within(ladder).getByRole("button", { name: /Admin/ }));

    await waitFor(() =>
      expect(connection.dispatcher.sendAndWait).toHaveBeenCalledWith(
        expect.objectContaining({
          identityAdmin: expect.objectContaining({
            setUserRole: { userId: "v1:identity:user:ada", role: "admin" },
          }),
        }),
        undefined,
      ),
    );
    await waitFor(() => {
      const held = within(screen.getByRole("list", { name: /The cluster's roles/ })).getByText("held");
      expect(held.closest("button")?.textContent).toContain("Admin");
    });
  });

  it("leaves the old rung marked when the write is refused, and says why in the server's words", async () => {
    const connection = seed();
    connection.dispatcher.sendAndWait.mockResolvedValue(
      adminRefusal("an inviter cannot grant above their own role"),
    );
    await openAda(connection);

    const ladder = screen.getByRole("list", { name: /The cluster's roles/ });
    await click(within(ladder).getByRole("button", { name: /Admin/ }));

    expect(await screen.findByText(/an inviter cannot grant above their own role/)).toBeTruthy();
    const held = within(screen.getByRole("list", { name: /The cluster's roles/ })).getByText("held");
    expect(held.closest("button")?.textContent).toContain("Member");
  });
});

// ===========================================================================
// THE ACTS, AND THE STATE THEY FOLLOW (rule 12)
// ===========================================================================

describe("the action bar", () => {
  it("offers Deactivate on an active person", async () => {
    const view = await openAda(seed());
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByText("Active")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Deactivate" })).toBeTruthy();
    expect(within(bar).queryByRole("button", { name: "Reactivate" })).toBeNull();
    view.unmount();
  });

  it("offers Reactivate, and nothing else, on a deactivated one", async () => {
    const retired = userRow({ id: "v1:identity:user:ada", displayName: "Ada", active: false });
    // The roster hides deactivated people unless the setting says otherwise
    // (rule 4), so this one is opened with the preference on -- which is the
    // only way somebody reaches a retired account's page at all.
    const view = await openAda(
      seed({ searchUsers: [retired], byId: { "v1:identity:user:ada": retired } }),
      "owner",
      true,
    );
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByText("Deactivated")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Reactivate" })).toBeTruthy();
    expect(within(bar).queryByRole("button", { name: "Deactivate" })).toBeNull();
    view.unmount();
  });

  it("does not offer Reset sign-in policy to somebody who already has links", async () => {
    // AN ILLEGAL ACT IS ABSENT, NEVER DISABLED. Re-enabling links for somebody
    // who already has them is a write with no meaning.
    const view = await openAda(seed());
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button", { name: "Reset sign-in policy" })).toBeNull();
    view.unmount();
  });

  it("offers it to somebody actually on passkey_only", async () => {
    const locked = userRow({
      id: "v1:identity:user:ada",
      displayName: "Ada",
      signInPolicy: "passkey_only",
    });
    const view = await openAda(
      seed({ searchUsers: [locked], byId: { "v1:identity:user:ada": locked } }),
    );
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByRole("button", { name: "Reset sign-in policy" })).toBeTruthy();
    view.unmount();
  });

  it("gives a peer the state and no acts, and says the row is read-only", async () => {
    // BRAND-NEW STATE under rank-visible reads: a row you cannot edit used to
    // be a row you could not SEE, so an editor that simply refused to save
    // would read as a bug.
    const peer = userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "owner" });
    await openAda(seed({ searchUsers: [peer], byId: { "v1:identity:user:ada": peer } }), "admin");
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button", { name: "Deactivate" })).toBeNull();
    expect(screen.getByText(/Read-only/)).toBeTruthy();
  });
});

// ===========================================================================
// THE GROUPS PANEL
// ===========================================================================

describe("the groups panel", () => {
  it("reads this person's memberships and names why each one is there", async () => {
    const connection = seed({
      groupsForUser: {
        "v1:identity:user:ada": [
          membershipRow({
            id: "m1",
            groupId: "g-acme",
            userId: "v1:identity:user:ada",
            origin: "domain",
          }),
        ],
      },
    });
    await openAda(connection);

    expect(await screen.findByText("joined on @acme.com")).toBeTruthy();
    expect(connection.query.groupsForUser.mock.calls[0]?.[0]).toMatchObject({
      userId: "v1:identity:user:ada",
    });
  });

  it("says the standing rule instead of rows for staff", async () => {
    // The standing staff are a RULE, not rows (epic memql#5165, D6): no query
    // returns those memberships, so a list here would be empty or invented.
    const staff = userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "developer" });
    await openAda(seed({ searchUsers: [staff], byId: { "v1:identity:user:ada": staff } }));
    expect(await screen.findByText(/In every account's group, standing/)).toBeTruthy();
  });
});
