import { accountAccessFixture } from "../accountAccessFixture";
import { fireEvent } from "@testing-library/react";
import { readTheBar } from "../readTheBar";
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
  roleRow,
  withSession,
} = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");

type Conn = ReturnType<typeof fakeConnection>;

const GROUP_CONCEPT = "v1:identity:group";

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
    await readTheBar(); await click(screen.getByRole("button", { name: "Next" }));
    await click(screen.getByRole("combobox", { name: "Organization this group belongs to" }));
    await click(screen.getByRole("option", { name: "Acme" }));
    await readTheBar(); await click(screen.getByRole("button", { name: "Next" }));
    await readTheBar(); await click(screen.getByRole("button", { name: "Create group" }));

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

describe("account group management", () => {
  const GROUP = groupRow({ id: "reviewers", name: "Reviewers", kind: "custom", accountId: "acct-acme" });
  const ada = { id: "ada", name: "Ada", email: "ada@example.test", role: "member", apps: [] };
  const grace = { id: "grace", name: "Grace", email: "grace@example.test", role: "viewer", apps: [] };
  async function open(state = accountAccessFixture({ accountId: "acct-acme" })) {
    const conn = seed({ groupsAll: [GROUP], accountAccess: state as never });
    mount(conn);
    await click(await screen.findByRole("button", { name: /Reviewers/ }));
    await screen.findByRole("list", { name: "Account members" });
    return conn;
  }
  it("uses the reduced account roster and shows only this group's members", async () => {
    const conn = await open(accountAccessFixture({ accountId: "acct-acme", people: [ada, grace], groups: [{ id: "reviewers", name: "Reviewers", kind: "custom", apps: [], userIds: ["ada"] }] }));
    const list = screen.getByRole("list", { name: "Account members" });
    expect(within(list).getByText("Ada")).toBeTruthy();
    expect(within(list).queryByText("Grace")).toBeNull();
    expect(conn.query.executeNamed.mock.calls.some(([name]) => name === "accountAccess")).toBe(true);
    expect(screen.queryByRole("combobox", { name: "Role for Ada" })).toBeNull();
  });
  it("adds multiple people in one account-scoped request", async () => {
    const conn = await open(accountAccessFixture({ accountId: "acct-acme", people: [ada, grace], groups: [{ id: "reviewers", name: "Reviewers", kind: "custom", apps: [], userIds: [] }] }));
    const picker = screen.getByRole("combobox", { name: "People" });
    for (const name of ["Ada", "Grace"]) {
      fireEvent.change(picker, { target: { value: name } });
      await click(screen.getByRole("option", { name: new RegExp(name) }));
    }
    await click(screen.getByRole("button", { name: "Add selected people" }));
    await waitFor(() => expect(conn.query.executeNamed.mock.calls.filter(([name]) => name === "accountAccessUpdate")).toHaveLength(1));
    const call = conn.query.executeNamed.mock.calls.find(([name]) => name === "accountAccessUpdate")![1];
    expect(call).toContain('operation: "groupMembers"'); expect(call).toContain('groupId: "reviewers"');
    expect(call).toContain('"ada"'); expect(call).toContain('"grace"');
  });
  it("offers only the account's apps when assigning group access", async () => {
    await open(accountAccessFixture({ accountId: "acct-acme", apps: ["app:deployables"], groups: [{ id: "reviewers", name: "Reviewers", kind: "custom", apps: [], userIds: [] }] }));
    await click(screen.getByRole("button", { name: "Assign apps to group" }));
    fireEvent.focus(screen.getByRole("combobox", { name: "Apps" }));
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(1);
    expect(options[0]?.textContent).toMatch(/Deployables/);
    expect(screen.queryByRole("button", { name: "Edit apps" })).toBeNull();
  });
  it("does not offer ownership transfer from a custom group", async () => {
    await open();
    expect(screen.queryByRole("button", { name: "Transfer ownership" })).toBeNull();
  });
  it("keeps an account group tied to its account lifecycle", async () => {
    const group = groupRow({ id: "acct-acct-acme", name: "Acme people", kind: "account", accountId: "acct-acme" });
    const conn = seed({ groupsAll: [group] }); mount(conn);
    await click(await screen.findByRole("button", { name: /Acme people/ }));
    await screen.findByText("Account owner");
    expect(screen.queryByRole("button", { name: "Archive group" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit group" })).toBeNull();
    expect(screen.getByRole("button", { name: "Open account" })).toBeTruthy();
  });
});
