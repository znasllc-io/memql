import { readTheBar } from "../readTheBar";
import { act, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

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
  groupAccessRow,
  groupRow,
  invitationRow,
  membershipRow,
  roleRow,
  subjectGrantRow,
  userRow,
  withSession,
} = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { LocalDesktopStore } = await import("../../src/system/store");
const { installSeededAccess, seededAccessFor } = await import("../seededAccess");
const { setEffectiveCapabilities } = await import("../../src/system/roles");

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

describe("an organization's apps on its group page", () => {
  // WHAT THE ORGANIZATION'S PEOPLE MAY OPEN IS SET ON ITS GROUP (access
  // clarity, 2026-11-06): the group's allow grants on apps, each app a row with
  // the parts of it allowed beneath, each with Remove; Add an app offers the
  // catalog's apps the group does not already allow. The rows are the engine's
  // (`effectiveAccessForSubject`); the grant a Remove takes is the group's own
  // (`grantsForSubject`), matched on verb and resource.
  const GROUP = groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme" });
  const APPS = [
    grantRow("owner", "read", "app:email"),
    grantRow("owner", "read", "app:deployables"),
    grantRow("owner", "execute", "app:deployables/reviews"),
    grantRow("owner", "execute", "app:deployables/deploy"),
    grantRow("owner", "read", "app:files"),
    grantRow("owner", "read", "app:logs"),
  ];

  function acmeSeed(over: Parameters<typeof fakeConnection>[0] = {}) {
    return seed({
      activeCapabilities: [...GRANTS, ...APPS],
      groupsAll: [GROUP],
      membersOfGroup: { "g-acme": [] },
      effectiveAccess: {
        "group:g-acme": [
          groupAccessRow("g-acme", "acct-acme", "read", "app:deployables"),
          groupAccessRow("g-acme", "acct-acme", "execute", "app:deployables/reviews"),
          // A deny is not an app its people can open: Settings > Access shows it.
          groupAccessRow("g-acme", "acct-acme", "read", "app:logs", false),
        ],
      },
      grantsForSubject: {
        "group:g-acme": [
          subjectGrantRow({ id: "grant-door", subjectKind: "group", subjectId: "g-acme", verb: "read", resourceType: "app:deployables" }),
          subjectGrantRow({ id: "grant-reviews", subjectKind: "group", subjectId: "g-acme", verb: "execute", resourceType: "app:deployables/reviews" }),
          subjectGrantRow({ id: "grant-logs", subjectKind: "group", subjectId: "g-acme", verb: "read", resourceType: "app:logs", effect: "deny" }),
        ],
      },
      ...over,
    });
  }

  function memStorage(): Pick<Storage, "getItem" | "setItem"> {
    const data = new Map<string, string>();
    return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
  }

  /**
   * The group page inside the shell, which is what names the apps, as an
   * owner holding `held` -- the owner's whole set unless a case takes away
   * the one capability it is about.
   */
  async function openAcmeInShell(connection: Conn, held = seededAccessFor("owner"), store = memoryStore()) {
    setEffectiveCapabilities(held);
    h.connection = connection;
    const view = renderApp(
      withSession(
        <OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{ cols: 12, rows: 8 }} store={new LocalDesktopStore(memStorage())}>
          <UsersApp sectionId="groups" navigate={() => {}} askContext={() => {}} store={store} />
        </OsProvider>,
      ),
    );
    await click(await screen.findByRole("button", { name: /Acme/ }));
    await screen.findByText("Members");
    return view;
  }

  /** What the list says, row by row: each name, and the acts beside it. */
  function appRows(list: HTMLElement): string[] {
    return within(list).getAllByRole("listitem").map((item) => {
      const name = item.querySelector(".os-row-name")?.textContent ?? "";
      const acts = within(item).queryAllByRole("button").map((b) => b.textContent ?? "");
      return [`${item.hasAttribute("data-part") ? "  " : ""}${name}`, ...acts].join(" | ");
    });
  }

  afterEach(() => {
    installSeededAccess("owner");
  });

  it("an account group lists the apps its people can open and adds one", async () => {
    const connection = acmeSeed();
    const view = await openAcmeInShell(connection);
    const list = await screen.findByRole("list", { name: "Apps its people can open" });
    expect(connection.query.effectiveAccessForSubject.mock.calls[0]?.[0]).toEqual({ subjectKind: "group", subjectId: "g-acme" });
    // Deployables, and the reviews part beneath it; the denied Logs is not here.
    expect(appRows(list)).toEqual(["Deployables | Remove", "  reviews | Remove"]);

    // ADD AN APP: the catalog's apps the group does not already allow.
    await click(screen.getByRole("button", { name: "Add an app" }));
    const picker = await screen.findByRole("listbox", { name: "Apps to add" });
    const offered = within(picker).getAllByRole("option").map((o) => o.getAttribute("aria-label") ?? "");
    expect(offered).toContain("Email");
    expect(offered).toContain("Files");
    expect(offered).toContain("Logs");
    expect(offered).not.toContain("Deployables");
    await click(within(picker).getByRole("option", { name: "Email" }));
    await waitFor(() => expect(connection.query.grantSet).toHaveBeenCalledTimes(1));
    expect(connection.query.grantSet.mock.calls[0]?.[0]).toEqual({ subjectKind: "group", subjectId: "g-acme", verb: "read", resourceType: "app:email", effect: "allow" });
    // What the cluster holds is read again, and is the list.
    await waitFor(() => expect(appRows(screen.getByRole("list", { name: "Apps its people can open" }))).toEqual(["Email | Remove", "Deployables | Remove", "  reviews | Remove"]));
    expect(screen.queryByRole("listbox", { name: "Apps to add" })).toBeNull();
    // The keyboard is back on the control the picker was opened from.
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add an app" }));

    // REMOVE takes the group's own grant on that pair.
    const reviews = within(screen.getByRole("list", { name: "Apps its people can open" })).getAllByRole("listitem")[2]!;
    await click(within(reviews).getByRole("button", { name: "Remove reviews from Acme" }));
    await waitFor(() => expect(connection.query.grantRevoke).toHaveBeenCalledWith({ grantId: "grant-reviews" }));
    await waitFor(() => expect(appRows(screen.getByRole("list", { name: "Apps its people can open" }))).toEqual(["Email | Remove", "Deployables | Remove"]));
    view.unmount();
  });

  it("says why when the cluster will not read the group's grants, and offers nothing", async () => {
    const connection = acmeSeed({
      effectiveAccess: { "group:g-acme": new Error("effectiveAccessForSubject: MemQL engine failed to execute query. Details: access_unknown_subject: no active group by that id") },
    });
    const view = await openAcmeInShell(connection);
    const panel = screen.getByRole("region", { name: "Organization access" });
    expect(await within(panel).findByText(/access_unknown_subject/)).toBeTruthy();
    expect(within(panel).queryByRole("list", { name: "Apps its people can open" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: "Add an app" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: /^Remove/ })).toBeNull();
    view.unmount();
  });

  /** The owner's set without one (verb, resource) pair. */
  const ownerWithout = (verb: string, resource: string) => seededAccessFor("owner").filter((c) => !(c.verb === verb && c.resource === resource));

  it("shows the apps without acts to someone who reads access but writes no grant", async () => {
    // A developer opens Settings > Access and holds no update on principal,
    // which every grant write takes: the acts are absent, never disabled.
    const view = await openAcmeInShell(acmeSeed(), ownerWithout("update", "principal"));
    const list = await screen.findByRole("list", { name: "Apps its people can open" });
    expect(appRows(list)).toEqual(["Deployables", "  reviews"]);
    expect(screen.queryByRole("button", { name: "Add an app" })).toBeNull();
    view.unmount();
  });

  it("is not on an archived organization group's page, which grants nobody anything", async () => {
    // The engine refuses to answer for an archived group (access_unknown_subject),
    // and an archived group opens nothing for anyone: no list to read, so no
    // warning about a list that could not be.
    const archived = groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme", status: "archived" });
    const connection = acmeSeed({
      groupsAll: [archived],
      effectiveAccess: { "group:g-acme": new Error("effectiveAccessForSubject: MemQL engine failed to execute query. Details: access_unknown_subject: no active group by that id") },
    });
    const view = await openAcmeInShell(connection, seededAccessFor("owner"), memoryStore({ showArchivedGroups: true }));
    const panel = screen.getByRole("region", { name: "Organization access" });
    expect(within(panel).queryByText("Apps its people can open")).toBeNull();
    expect(within(panel).queryByText(/access_unknown_subject/)).toBeNull();
    expect(within(panel).queryByText("What this group grants could not be read.")).toBeNull();
    expect(connection.query.effectiveAccessForSubject).not.toHaveBeenCalled();
    // Nor a way to manage what it grants: Settings > Access would open on a
    // group the engine refuses to answer for.
    expect(within(panel).queryByRole("button", { name: "Manage app access" })).toBeNull();
    view.unmount();
  });

  it("offers Manage app access on an active organization group", async () => {
    const view = await openAcmeInShell(acmeSeed());
    const panel = screen.getByRole("region", { name: "Organization access" });
    expect(within(panel).getByRole("button", { name: "Manage app access" })).toBeTruthy();
    view.unmount();
  });

  it("is not on the page for someone who does not manage access", async () => {
    // Shown only to those who hold Settings > Access, as the button beside it is.
    const connection = acmeSeed();
    const view = await openAcmeInShell(connection, ownerWithout("read", "app:settings/access"));
    expect(screen.getByRole("region", { name: "Organization access" })).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Apps its people can open" })).toBeNull();
    expect(connection.query.effectiveAccessForSubject).not.toHaveBeenCalled();
    view.unmount();
  });
});
