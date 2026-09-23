import { screen, waitFor, within } from "@testing-library/react";
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
const { chooseOption } = await import("../selectControl");

type Conn = ReturnType<typeof fakeConnection>;

const USER_CONCEPT = "v1:identity:user";
const INVITATION_CONCEPT = "v1:identity:invitation";

const LADDER = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
];

const GRANTS = [
  grantRow("owner", "read", "principal"),
  grantRow("owner", "create", "principal"),
  grantRow("owner", "update", "principal"),
  grantRow("owner", "delete", "principal"),
  grantRow("owner", "create", "group"),
  grantRow("owner", "update", "group"),
  grantRow("owner", "read", "role"),
  grantRow("owner", "create", "role"),
  grantRow("owner", "update", "role"),
  grantRow("owner", "delete", "role"),
  grantRow("owner", "create", "admission"),
  grantRow("admin", "read", "principal"),
  grantRow("admin", "create", "principal"),
  grantRow("admin", "update", "principal"),
  grantRow("admin", "create", "group"),
  grantRow("admin", "update", "group"),
  grantRow("admin", "create", "admission"),
  grantRow("developer", "read", "principal"),
  grantRow("developer", "create", "admission"),
];

function mount(connection: Conn, sectionId = "people", role = "owner") {
  h.connection = connection;
  return renderApp(
    withSession(
      <UsersApp
        sectionId={sectionId}
        navigate={() => {}}
        askContext={() => {}}
        store={memoryStore()}
      />,
      { role },
    ),
  );
}

function seed(extra: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [accountRow({ id: "self", name: "Our company" })],
    ...extra,
  });
}

// ===========================================================================
// ONE LIST, TWO FEEDS
// ===========================================================================
// An invitation is a person who has not arrived (design record, D1). The app
// used to answer "who is on this cluster" in two sections, so somebody looking
// for a colleague had to know whether that colleague had accepted yet -- which
// is the one thing they were trying to find out.

describe("the roster", () => {
  it("joins people and invitations into one list", async () => {
    mount(
      seed({
        searchUsers: [
          userRow({ id: "v1:identity:user:ada", displayName: "Ada", primaryEmail: "ada@example.com", role: "admin" }),
        ],
        pendingUserInvitations: [
          invitationRow({ id: "v1:identity:invitation:i1", inviteeEmail: "kit@example.com", inviteeRole: "user" }),
        ],
      }),
    );

    const person = await screen.findByRole("button", { name: /Ada/ });
    expect(within(person).getByText("ada@example.com")).toBeTruthy();

    const invited = await screen.findByRole("button", { name: /kit@example.com/ });
    expect(within(invited).getByText("Invited")).toBeTruthy();
  });

  it("moves an accepted invitation off the list live, with no refetch", async () => {
    // `v1:identity:invitation` broadcasts BOTH created and updated, which is
    // what lets an acceptance take a row off this list on its own: the write
    // sets `status: "accepted"`, the update arrives, and the row stops
    // satisfying the read's own membership predicate.
    const connection = seed({
      pendingUserInvitations: [
        invitationRow({ id: "v1:identity:invitation:i1", inviteeEmail: "kit@example.com" }),
      ],
    });
    mount(connection);
    await screen.findByRole("button", { name: /kit@example.com/ });

    connection.subscriptions.emit(INVITATION_CONCEPT, {
      id: "v1:identity:invitation:i1",
      status: "accepted",
      kind: "user",
      active: true,
    } as never);

    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /kit@example.com/ })).toBeNull(),
    );
  });

  it("rings the cue on a rename and not on a heartbeat", async () => {
    // A HEARTBEAT IS NOT NEWS. `lastSeenAt` moves for every person forever, so
    // naming it in the fingerprint would turn the list into a strobe on a
    // timer -- the standing badge the cue exists not to be.
    const connection = seed({
      searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "user" })],
    });
    mount(connection);
    await screen.findByRole("button", { name: /Ada/ });

    connection.subscriptions.emit(USER_CONCEPT, {
      id: "v1:identity:user:ada",
      displayName: "Ada",
      role: "user",
      lastSeenAt: new Date().toISOString(),
    } as never);
    await waitFor(() => expect(document.querySelector('[data-arrival="updated"]')).toBeNull());

    connection.subscriptions.emit(USER_CONCEPT, {
      id: "v1:identity:user:ada",
      displayName: "Ada Lovelace",
      role: "user",
    } as never);
    await waitFor(() => expect(document.querySelector('[data-arrival="updated"]')).toBeTruthy());
  });

  it("renders a refused read in surface, with the engine's own words", async () => {
    // Not a toast, and not an empty list: somebody who reached this surface
    // out-of-band has to read WHY rather than conclude the cluster is empty.
    const connection = seed();
    connection.query.searchUsers = vi.fn(async () => {
      throw new Error("searchUsers: reading the directory is admin and above");
    });
    mount(connection);
    expect(await screen.findByText(/did not return its people/i)).toBeTruthy();
    expect(screen.getByText(/reading the directory is admin and above/)).toBeTruthy();
  });

  it("hides deactivated people and says the setting is why", async () => {
    mount(
      seed({
        searchUsers: [
          userRow({ id: "v1:identity:user:old", displayName: "Retired", active: false }),
        ],
      }),
    );
    expect(await screen.findByText(/turn on deactivated people/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Retired/ })).toBeNull();
  });
});

// ===========================================================================
// THE GROUP FACET COSTS ONE READ
// ===========================================================================

describe("refining by group", () => {
  it("reads exactly the picked group's members, and nothing when none is picked", async () => {
    const connection = seed({
      searchUsers: [
        userRow({ id: "v1:identity:user:ada", displayName: "Ada" }),
        userRow({ id: "v1:identity:user:kit", displayName: "Kit" }),
      ],
      groupsAll: [groupRow({ id: "g1", name: "Acme", accountId: "acct-1", kind: "account" })],
      membersOfGroup: {
        g1: [membershipRow({ id: "m1", groupId: "g1", userId: "v1:identity:user:ada" })],
      },
    });
    mount(connection);
    await screen.findByRole("button", { name: /Ada/ });

    // Nothing has been picked, so no membership has been read at all.
    const membersCalls = () => connection.query.membersOfGroup.mock.calls;
    expect(membersCalls()).toHaveLength(0);

    await click(screen.getByRole("button", { name: /Refine/ }));
    // Driven the way a person drives it: the facet is the kit's own listbox,
    // so a `change` event fired at the trigger would reach nothing.
    const { act } = await import("@testing-library/react");
    chooseOption(screen.getByLabelText("Group"), "Acme");
    await act(async () => {});

    await waitFor(() => expect(membersCalls().length).toBeGreaterThan(0));
    expect(membersCalls()[0]?.[0]).toMatchObject({ groupId: "g1" });
    await waitFor(() => expect(screen.queryByRole("button", { name: /Kit/ })).toBeNull());
    expect(screen.getByRole("button", { name: /Ada/ })).toBeTruthy();
  });
});

// ===========================================================================
// RULE 11
// ===========================================================================

describe("opening a person", () => {
  it("replaces the list rather than appending a panel beneath it", async () => {
    mount(
      seed({
        searchUsers: [
          userRow({ id: "v1:identity:user:ada", displayName: "Ada", primaryEmail: "ada@example.com" }),
        ],
        byId: {
          "v1:identity:user:ada": userRow({
            id: "v1:identity:user:ada",
            displayName: "Ada",
            primaryEmail: "ada@example.com",
          }),
        },
      }),
    );
    await click(await screen.findByRole("button", { name: /Ada/ }));

    // ONE HEAD. Two stacked Heads in one scroller is the tell that neither of
    // the shell's two detail patterns was adopted, and this app was one of the
    // two that appended a panel.
    await waitFor(() => expect(document.querySelectorAll(".os-head")).toHaveLength(1));
    expect(screen.queryByRole("button", { name: /Refine/ })).toBeNull();
    expect(screen.getByText("Role")).toBeTruthy();
  });
});


describe("authorized roster counts", () => {
  it("waits for both reads and counts only the visible people and invitations", async () => {
    const connection = seed({
      searchUsers: [
        userRow({ id: "active", displayName: "Active colleague" }),
        userRow({ id: "inactive", displayName: "Inactive colleague", active: false }),
      ],
      pendingUserInvitations: [invitationRow({ id: "waiting", inviteeEmail: "waiting@example.com" })],
    });
    const view = mount(connection);
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
    await waitFor(() => expect(view.container.querySelector(".os-head-meta")?.textContent).toBe("2"));
    expect(view.container.querySelectorAll(".os-record-list .os-record-row")).toHaveLength(2);
    expect(screen.queryByText("Inactive colleague")).toBeNull();
  });

  it("does not count a partial roster when the invitations read is denied", async () => {
    const connection = seed({ searchUsers: [userRow({ id: "active", displayName: "Active colleague" })] });
    connection.query.pendingUserInvitations.mockRejectedValue(new Error("invitation read denied"));
    const view = mount(connection);
    await screen.findByText(/invitation read denied/);
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
  });
});
