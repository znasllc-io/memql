import { act, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const {
  accountRow,
  fakeConnection,
  grantRow,
  groupRow,
  invitationRow,
  membershipRow,
  roleRow,
  userRow,
  withSession,
} = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");

type Conn = ReturnType<typeof fakeConnection>;

const GROUP_CONCEPT = "v1:identity:group";
const MEMBERSHIP_CONCEPT = "v1:identity:groupMembership";

const LADDER = [
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
];

const GRANTS = [
  grantRow("owner", "read", "principal"),
  grantRow("owner", "create", "group"),
  grantRow("owner", "update", "group"),
];

const ACME = accountRow({ id: "acct-acme", name: "Acme", domain: "acme.com" });

function seed(extra: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [ACME],
    ...extra,
  });
}

function mount(connection: Conn, role = "owner") {
  h.connection = connection;
  return renderApp(
    withSession(
      <UsersApp sectionId="groups" navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
      { role },
    ),
  );
}

describe("the groups list", () => {
  it("opens delegated organization membership without reading the global directory", async () => {
    const connection = seed({
      groupsAll: [groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme" })],
      groupPeople: [userRow({ id: "u-client", displayName: "Client colleague" })],
      membersOfGroup: { "g-acme": [membershipRow({ id: "membership-client", groupId: "g-acme", userId: "u-client" })] },
    });
    const view = mount(connection, "acme-admin");
    await click(await screen.findByRole("button", { name: /Acme/ }));
    expect(await screen.findByText("Client colleague")).toBeTruthy();
    expect(connection.query.groupPeople.mock.calls[0]?.[0]).toEqual({ groupId: "g-acme" });
    expect(connection.query.searchUsers).not.toHaveBeenCalled();
    expect(connection.query.pendingUserInvitations).not.toHaveBeenCalled();
    view.unmount();
  });

  it("takes a new group from the broadcast rather than inserting it locally", async () => {
    // NOTHING IS INSERTED LOCALLY. The row arrives on its own broadcast with
    // the arrival cue, which is what makes the new group appear the same way
    // it appears in everybody else's window -- and what stops this one showing
    // a group the cluster refused to write.
    const connection = seed({ groupsAll: [] });
    const view = mount(connection);
    await screen.findByText("Groups");

    await click(screen.getByRole("button", { name: "New group" }));
    const { fireEvent } = await import("@testing-library/react");
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Reviewers" } });
    });
    await click(screen.getByRole("button", { name: "Create group" }));

    await waitFor(() => expect(connection.query.groupCreate.mock.calls).toHaveLength(1));
    expect(connection.query.groupCreate.mock.calls[0]?.[0]).toMatchObject({ name: "Reviewers" });
    // Back on the list, and the row is NOT there until the cluster says so.
    await screen.findByRole("button", { name: "New group" });
    expect(screen.queryByRole("button", { name: /Reviewers/ })).toBeNull();

    await act(async () => {
      connection.subscriptions.emit(
        GROUP_CONCEPT,
        { id: "g-new", name: "Reviewers", kind: "custom", status: "active" } as never,
        "NODE_CREATED",
      );
    });
    expect(await screen.findByRole("button", { name: /Reviewers/ })).toBeTruthy();
    view.unmount();
  });

  it("hides archived groups and says the setting is why", async () => {
    const view = mount(
      seed({ groupsAll: [groupRow({ id: "g-old", name: "Old crew", status: "archived" })] }),
    );
    expect(await screen.findByText(/turn on archived groups/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Old crew/ })).toBeNull();
    view.unmount();
  });
});

describe("a group's page", () => {
  const GROUP = groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme" });

  async function openAcme(connection: Conn, role = "owner") {
    const view = mount(connection, role);
    await click(await screen.findByRole("button", { name: /Acme/ }));
    await screen.findByText("Members");
    return view;
  }

  it("lists members live, on the membership event", async () => {
    const connection = seed({
      groupsAll: [GROUP],
      searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada" })],
      membersOfGroup: { "g-acme": [] },
    });
    const view = await openAcme(connection);
    expect(screen.getByText(/Nobody in this group yet/)).toBeTruthy();

    await act(async () => {
      connection.subscriptions.emit(
        MEMBERSHIP_CONCEPT,
        membershipRow({ id: "m1", groupId: "g-acme", userId: "v1:identity:user:ada" }) as never,
        "NODE_CREATED",
      );
    });
    expect(await screen.findByText("Ada")).toBeTruthy();

    // A membership in ANOTHER group must not fold into this page: the
    // subscription is scoped by concept and `inScope` is what scopes it by
    // argument.
    await act(async () => {
      connection.subscriptions.emit(
        MEMBERSHIP_CONCEPT,
        membershipRow({ id: "m2", groupId: "g-other", userId: "v1:identity:user:kit" }) as never,
        "NODE_CREATED",
      );
    });
    expect(screen.queryByText("v1:identity:user:kit")).toBeNull();
    view.unmount();
  });

  it("keeps Archive off an account-kind group while its client is active, and says what to do", async () => {
    // AN ILLEGAL ACT IS ABSENT, NEVER DISABLED (rule 12), and the engine's own
    // `group_account_active` refuses the same write.
    const view = await openAcme(seed({ groupsAll: [GROUP], membersOfGroup: { "g-acme": [] } }));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button", { name: "Archive" })).toBeNull();
    expect(screen.getByText("Archive the account to archive this group.")).toBeTruthy();
    view.unmount();
  });

  it("offers Archive on a custom group tied to nobody", async () => {
    const connection = seed({
      groupsAll: [groupRow({ id: "g-rev", name: "Reviewers" })],
      membersOfGroup: { "g-rev": [] },
    });
    const view = mount(connection);
    await click(await screen.findByRole("button", { name: /Reviewers/ }));
    await screen.findByText("Members");
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByRole("button", { name: "Archive" })).toBeTruthy();
    view.unmount();
  });

  it("reads the domain note from the client's row, in both states", async () => {
    // The group holds no domain state: the rule lives on the client's row, and
    // a second copy here would be a second answer to "does joining apply".
    const off = await openAcme(seed({ groupsAll: [GROUP], membersOfGroup: { "g-acme": [] } }));
    expect(screen.getByText("Joining is off.")).toBeTruthy();
    off.unmount();

    const on = await openAcme(
      seed({
        groupsAll: [GROUP],
        membersOfGroup: { "g-acme": [] },
        clientAccountsAll: [
          accountRow({
            id: "acct-acme",
            name: "Acme",
            domain: "acme.com",
            domainStatus: "verified",
            joinOnDomain: true,
          }),
        ],
      }),
    );
    expect(screen.getByText("Joins on @acme.com.")).toBeTruthy();
    on.unmount();
  });

  it("draws the standing staff as a band with no controls", async () => {
    // The standing staff are a RULE, not rows: no query returns them, so the
    // band comes from the roster this window already holds and offers nothing
    // to remove -- there is no row to remove.
    const view = await openAcme(
      seed({
        groupsAll: [GROUP],
        membersOfGroup: { "g-acme": [] },
        searchUsers: [userRow({ id: "v1:identity:user:dev", displayName: "Dev", role: "developer" })],
      }),
    );
    const band = screen.getByRole("region", { name: /Managed by/ });
    expect(within(band).getByText("Everyone at developer and above, standing.")).toBeTruthy();
    expect(within(band).queryByRole("button")).toBeNull();
    view.unmount();
  });

  it("shows people invited into the group before they arrive", async () => {
    // There is no membership row until they accept, so these come from the
    // invitations feed.
    const view = await openAcme(
      seed({
        groupsAll: [GROUP],
        membersOfGroup: { "g-acme": [] },
        pendingUserInvitations: [
          invitationRow({ id: "i1", inviteeEmail: "kit@acme.com", groupIds: ["g-acme"] }),
        ],
      }),
    );
    const list = screen.getByRole("list", { name: /People invited into Acme/ });
    expect(within(list).getByText("kit@acme.com")).toBeTruthy();
    view.unmount();
  });
});
