import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { accountRow, fakeConnection, grantRow, groupRow, roleRow, userRow, withSession } =
  await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");
const { USERS_VIEW_KINDS } = await import("../../src/apps/users/views");

type Conn = ReturnType<typeof fakeConnection>;

// ===========================================================================
// ONE HEAD PER VIEW, NEVER TWO IN ONE SCROLLER (DESIGN.md rule 11)
// ===========================================================================
// Two stacked Heads is the TELL that neither of the shell's two detail
// patterns was adopted -- and this app was one of the two that appended a
// panel beneath the list it was selected from. Every view of every section is
// walked here rather than spot-checked, because the failure is a layout one:
// it renders, it works, and it is wrong at real size.

const LADDER = [
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
];

const GRANTS = [
  grantRow("owner", "read", "principal"),
  grantRow("owner", "update", "principal"),
  grantRow("owner", "create", "group"),
  grantRow("owner", "update", "group"),
  grantRow("owner", "create", "role"),
  grantRow("owner", "update", "role"),
  grantRow("owner", "create", "admission"),
];

const ADA = userRow({ id: "v1:identity:user:ada", displayName: "Ada", primaryEmail: "ada@example.com" });
const GROUP = groupRow({ id: "g1", name: "Reviewers" });

function connection(): Conn {
  return fakeConnection({
    searchUsers: [ADA],
    byId: { "v1:identity:user:ada": ADA },
    groupsAll: [GROUP],
    membersOfGroup: { g1: [] },
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [accountRow({ id: "self", name: "Our company" })],
  });
}

function mount(section: string) {
  h.connection = connection();
  return renderApp(
    withSession(
      <UsersApp sectionId={section} navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
      { role: "owner" },
    ),
  );
}

function heads(): number {
  return document.querySelectorAll(".os-head").length;
}

describe("every view of the Users app", () => {
  it("declares its view kinds in one place, for this test to walk", () => {
    // THE POSITIVE THIS SUITE RESTS ON. Every case below asserts "there is
    // exactly one Head", and a suite that walked an empty list of views would
    // satisfy that vacuously.
    expect(USERS_VIEW_KINDS.people).toEqual(["list", "person", "invited", "invite"]);
    expect(USERS_VIEW_KINDS.groups).toEqual(["list", "group", "new"]);
    expect(USERS_VIEW_KINDS.roles).toEqual(["list", "role", "new"]);
  });

  it("renders one Head on the People list, and one on a person's page", async () => {
    const view = mount("people");
    await screen.findByRole("button", { name: /Ada/ });
    expect(heads()).toBe(1);

    await click(screen.getByRole("button", { name: /Ada/ }));
    await waitFor(() => expect(screen.getByText("Role")).toBeTruthy());
    expect(heads()).toBe(1);
    view.unmount();
  });

  it("renders one Head on the Invite rail", async () => {
    const view = mount("people");
    await click(await screen.findByRole("button", { name: "Invite" }));
    await screen.findByRole("heading", { name: "Invite somebody" });
    expect(heads()).toBe(1);
    view.unmount();
  });

  it("renders one Head on the Groups list, a group's page and New group", async () => {
    const view = mount("groups");
    await screen.findByRole("button", { name: /Reviewers/ });
    expect(heads()).toBe(1);

    await click(screen.getByRole("button", { name: /Reviewers/ }));
    await screen.findByText("Members");
    expect(heads()).toBe(1);
    view.unmount();

    const fresh = mount("groups");
    await click(await screen.findByRole("button", { name: "New group" }));
    await screen.findByRole("region", { name: "A new group" });
    expect(heads()).toBe(1);
    fresh.unmount();
  });

  it("renders one Head on the Roles list, a role's page and New role", async () => {
    const view = mount("roles");
    await screen.findByRole("list", { name: /This cluster's roles/ });
    expect(heads()).toBe(1);

    const list = screen.getByRole("list", { name: /This cluster's roles/ });
    await click(within(list).getByRole("button", { name: /Member/ }));
    await screen.findByText("Permissions");
    expect(heads()).toBe(1);
    view.unmount();

    const fresh = mount("roles");
    await click(await screen.findByRole("button", { name: "New role" }));
    // The rail's stop label and the select's own label both read "Start from",
    // which is rule 7 working as intended: the stop names the question once
    // and the control inside it answers the same one.
    await screen.findByRole("list", { name: "What this role is" });
    expect(heads()).toBe(1);
    fresh.unmount();
  });

  it("renders one Head on Settings", async () => {
    const view = mount("settings");
    await screen.findByText("Users settings");
    expect(heads()).toBe(1);
    view.unmount();
  });
});
