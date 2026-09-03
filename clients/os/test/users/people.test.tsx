import { act, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read, and its provider dials a real
// socket. Replacing the READ is what lets the real LiveCollection, the real
// retain path and the real projections run under jsdom.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { fakeConnection, invitationRow, userRow, withSession } = await import("./harness");
const { LocalUsersSettingsStore } = await import("../../src/apps/users/settings");

type Conn = ReturnType<typeof fakeConnection>;

const USER_CONCEPT = "v1:identity:user";

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalUsersSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn, sectionId = "people", role = "owner") {
  h.connection = connection;
  return render(
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

describe("People, live", () => {
  it("renders the columns a person is identified by, from the searchUsers seed", async () => {
    const view = mount(
      fakeConnection({
        searchUsers: [
          userRow({
            id: "v1:identity:user:ada",
            displayName: "Ada",
            primaryEmail: "ada@example.com",
            role: "admin",
            signInPolicy: "passkey_only",
          }),
        ],
      }),
    );

    const row = await screen.findByRole("button", { name: /Ada/ });
    expect(within(row).getByText("ada@example.com")).toBeTruthy();
    expect(within(row).getByText("admin")).toBeTruthy();
    // passkey_only earns a chip; `any` deliberately does not.
    expect(within(row).getByText("passkey only")).toBeTruthy();
    view.unmount();
  });

  // ===========================================================================
  // WHAT A DEVELOPER SEES IN A PERSON PANEL
  // ===========================================================================
  // Developer reaches this app because it holds create-on-admission: it may
  // invite people and mint enrolment links. It holds NO verb on `principal`,
  // so changing a role and re-enabling sign-in links are refused server-side
  // every time by adminops' unchanged owner/admin gate.
  //
  // Those two controls are therefore not rendered for a developer, and the
  // third is. Without these cases the app-level floor keeps passing while
  // offering a developer a role picker whose every option fails.

  it("offers a developer the enrolment link and no role picker", async () => {
    const view = mount(
      fakeConnection({
        searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "reader" })],
      }),
      "people",
      "developer",
    );

    const row = await screen.findByRole("button", { name: /Ada/ });
    act(() => row.click());

    expect(screen.queryByLabelText(/Cluster role for/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Re-enable sign-in links/ })).toBeNull();
    expect(screen.getByRole("button", { name: /Mint enrolment link/ })).toBeTruthy();
    view.unmount();
  });

  it("offers an admin the role picker the developer does not get", async () => {
    // The positive control. Without it the case above is satisfied by a panel
    // that renders no controls for anybody.
    const view = mount(
      fakeConnection({
        searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "reader" })],
      }),
      "people",
      "admin",
    );

    const row = await screen.findByRole("button", { name: /Ada/ });
    act(() => row.click());

    expect(screen.getByLabelText(/Cluster role for/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Mint enrolment link/ })).toBeTruthy();
    view.unmount();
  });

  it("shows the LiveState caption rather than an empty cluster while it seeds", async () => {
    const connection = fakeConnection({ searchUsers: [] });
    const view = mount(connection);
    // The caption exists at all -- a frozen list must never be mistaken for a
    // cluster with nobody in it.
    await waitFor(() => expect(connection.query.searchUsers).toHaveBeenCalled());
    view.unmount();
  });

  it("slides a newly created person in with the arrival cue", async () => {
    const connection = fakeConnection({
      searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada" })],
    });
    const view = mount(connection);
    await screen.findByRole("button", { name: /Ada/ });

    // The exemplar: somebody accepts an invitation while this is on screen.
    // `graph.node.created.v1:identity:user` is the ONE broadcast rule this
    // concept carries, and it is what delivers this.
    act(() => {
      connection.subscriptions.emit(
        USER_CONCEPT,
        userRow({ id: "v1:identity:user:grace", displayName: "Grace" }),
        "NODE_CREATED",
      );
    });

    const row = await screen.findByRole("button", { name: /Grace/ });
    const item = row.closest("li");
    expect(item?.getAttribute("data-arrival")).toBe("added");
    expect(within(row).getByText("new")).toBeTruthy();
    // And no refetch: the seed ran once.
    expect(connection.query.searchUsers).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it("stays silent on a heartbeat -- lastSeenAt is not news", async () => {
    const connection = fakeConnection({
      searchUsers: [
        userRow({
          id: "v1:identity:user:ada",
          displayName: "Ada",
          lastSeenAt: "2026-08-01T00:00:00Z",
        }),
      ],
    });
    const view = mount(connection);
    await screen.findByRole("button", { name: /Ada/ });

    // The engine churns lastSeenAt. If it were in the fingerprint, every
    // person in the cluster would pulse forever on a timer -- the standing
    // badge the arrival cue exists not to be.
    act(() => {
      connection.subscriptions.emit(
        USER_CONCEPT,
        userRow({
          id: "v1:identity:user:ada",
          displayName: "Ada",
          lastSeenAt: new Date().toISOString(),
        }),
      );
    });

    const row = await screen.findByRole("button", { name: /Ada/ });
    expect(row.closest("li")?.getAttribute("data-arrival")).toBeNull();
    view.unmount();
  });

  it("pulses a row whose role a person would call changed", async () => {
    const connection = fakeConnection({
      searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "reader" })],
    });
    const view = mount(connection);
    await screen.findByRole("button", { name: /Ada/ });

    act(() => {
      connection.subscriptions.emit(
        USER_CONCEPT,
        userRow({ id: "v1:identity:user:ada", displayName: "Ada", role: "admin" }),
      );
    });

    await waitFor(() => {
      const row = screen.getByRole("button", { name: /Ada/ });
      expect(row.closest("li")?.getAttribute("data-arrival")).toBe("updated");
    });
    view.unmount();
  });

  it("hides deactivated people by default and lists them when asked", async () => {
    const rows = [
      userRow({ id: "v1:identity:user:ada", displayName: "Ada" }),
      userRow({ id: "v1:identity:user:retired", displayName: "Retired", active: false }),
    ];
    const view = mount(fakeConnection({ searchUsers: rows }));
    await screen.findByRole("button", { name: /Ada/ });
    expect(screen.queryByRole("button", { name: /Retired/ })).toBeNull();
    view.unmount();

    const withDeactivated = render(
      withSession(
        <UsersApp
          sectionId="people"
          navigate={() => {}}
          askContext={() => {}}
          store={(() => {
            const store = memoryStore();
            store.save({ version: 1, defaultSection: "people", showDeactivated: true });
            return store;
          })()}
        />,
      ),
    );
    expect(await screen.findByRole("button", { name: /Retired/ })).toBeTruthy();
    withDeactivated.unmount();
  });

  it("renders a refused read in surface, with the engine's own words", async () => {
    const connection = fakeConnection();
    connection.query.searchUsers = vi.fn(async () => {
      throw new Error("searchUsers: requiresOwnerOrAdmin");
    });
    const view = mount(connection);

    // Not a toast, and not an empty list: somebody who reached this surface
    // out-of-band has to read WHY rather than conclude the cluster is empty.
    expect(await screen.findByText(/did not return its people/i)).toBeTruthy();
    expect(screen.getByText(/requiresOwnerOrAdmin/)).toBeTruthy();
    view.unmount();
  });

  it("keeps the People list and the Invites list on one connection", async () => {
    // Both feeds seed from the SAME fake connection, which is what the
    // cross-list acceptance test in invites.test.tsx then relies on.
    const connection = fakeConnection({
      searchUsers: [userRow({ id: "v1:identity:user:ada", displayName: "Ada" })],
      pendingUserInvitations: [invitationRow({ id: "v1:identity:invitation:i1" })],
    });
    const view = mount(connection);
    await screen.findByRole("button", { name: /Ada/ });
    expect(connection.query.pendingUserInvitations).not.toHaveBeenCalled();
    view.unmount();
  });
});
