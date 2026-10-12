import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { chooseOption, openSelect } from "../selectControl";
import { installSeededAccess } from "../seededAccess";
import { readiness, verdict, withOs } from "../setup/harness";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../src/apps/email/EmailApp");
const { SettingsSection } = await import("../../src/apps/email/settings/SettingsSection");
const { ClusterEmailNotice } = await import("../../src/modules/connections/AzureEmailConnections");
const { AccountOptionsProvider } = await import("../../src/apps/accounts/tie");
const { useCampaignWrites } = await import("../../src/apps/email/actions");
const { EmailNavigationContext, useEmailRequests } = await import("../../src/apps/email/openRequest");
const { DEFAULT_EMAIL_SETTINGS, LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const { useEmailFeeds, useEmailRecordFeeds } = await import("../../src/apps/email/useEmail");
const { audienceRow, fakeConnection, peakSubscriptions, rowsResult, senderRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const SENDER = "v1:campaigns:senderIdentity";
const TEST_SETTINGS = "v1:campaigns:testSettings";

afterEach(() => {
  // A case that signs in as someone else puts the owner back, which is what the
  // rest of the suite renders as.
  installSeededAccess("owner");
});

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

const TWO_ORGANIZATIONS = [
  { id: "self", name: "Operator organization", status: "active" },
  { id: "org", name: "Client", status: "active" },
];

function mount(
  connection: Conn,
  options: { role?: string; settings?: Record<string, unknown>; readiness?: ReturnType<typeof readiness>; accountIds?: string[] } = {},
) {
  h.connection = connection;
  const store = memoryStore(options.settings);
  const view = render(
    withSession(
      <EmailApp sectionId="settings" navigate={() => {}} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />,
      { role: options.role, readiness: options.readiness, accountIds: options.accountIds },
    ),
  );
  return { view, store };
}

/** The unlabelled buttons a person could press that do nothing: disabled, and not because they are working. */
function deadButtons(): string[] {
  return screen
    .queryAllByRole("button")
    .filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true")
    .map((button) => button.textContent ?? "");
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barButtons = () => within(bar()).getAllByRole("button").map((button) => button.textContent);

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

describe("Email settings", () => {
  it("is titled Settings and names each subject once", async () => {
    const conn = fakeConnection();
    mount(conn);

    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    // One Head on the page: the subjects are headings beneath it.
    expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(1);
    for (const subject of ["Sending domain", "Senders", "Testing", "Package"]) {
      expect(await screen.findAllByRole("heading", { level: 4, name: subject })).toHaveLength(1);
    }
    // Preferences are a group, named by its legend, not a fifth heading.
    expect(screen.getByRole("group", { name: "Preferences" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: /Preferences/ })).toBeNull();

    // The page is not a second place that says what it is.
    expect(screen.queryByText("Email settings")).toBeNull();
    expect(screen.queryByText(/Campaign preferences/)).toBeNull();
    expect(screen.queryByRole("heading", { name: /Package configuration/ })).toBeNull();
  });

  it("asks for the organization once, and only when there is a choice", async () => {
    const one = fakeConnection();
    mount(one);
    await screen.findByRole("heading", { level: 4, name: "Senders" });
    expect(screen.queryByRole("combobox", { name: "Organization" })).toBeNull();
    cleanup();

    const two = fakeConnection({
      accounts: TWO_ORGANIZATIONS,
      audiences: [audienceRow({ id: "client-test", name: "Client reviewers", accountId: "org" })],
      senderIdentities: [
        senderRow({ id: "s-self", address: "ops@operator.example", accountId: "self" }),
        senderRow({ id: "s-org", address: "news@client.example", accountId: "org" }),
      ],
      testSettings: [{ id: "ts-org", accountId: "org", audienceId: "client-test", createdAt: "2026-09-01T00:00:00Z" }],
    });
    mount(two);

    // One field, not one per subject, and the operator's own organization is where it starts.
    expect(await screen.findAllByRole("combobox", { name: "Organization" })).toHaveLength(1);
    expect(await screen.findByText("ops@operator.example")).toBeTruthy();
    expect(screen.queryByText("news@client.example")).toBeNull();

    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");

    // Choosing it shows that organization's senders and reads its testing settings.
    expect(await screen.findByText("news@client.example")).toBeTruthy();
    expect(screen.queryByText("ops@operator.example")).toBeNull();
    await waitFor(() => expect(two.query.campaignTestSettings).toHaveBeenCalledWith({ accountId: "org" }, expect.anything()));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Client reviewers"));
    expect(screen.getAllByRole("combobox", { name: "Organization" })).toHaveLength(1);
  });

  it("waits for a choice when there are several organizations and no place to start", async () => {
    installSeededAccess("writer");
    mount(fakeConnection({ accounts: TWO_ORGANIZATIONS }), { role: "writer", accountIds: ["self", "org"] });

    const field = await screen.findByRole("combobox", { name: "Organization" });
    expect(field.textContent).toContain("Choose an organization");
    // Nothing that belongs to an organization is drawn for none, and what does not still is.
    expect(screen.queryByRole("heading", { level: 4, name: "Senders" })).toBeNull();
    expect(screen.queryByRole("heading", { level: 4, name: "Testing" })).toBeNull();
    expect(screen.getByRole("group", { name: "Preferences" })).toBeTruthy();

    chooseOption(field, "Client");
    expect(await screen.findByRole("heading", { level: 4, name: "Senders" })).toBeTruthy();
  });

  it("opens on the organization a request names, again each time it is asked", async () => {
    const conn = fakeConnection({
      accounts: TWO_ORGANIZATIONS,
      senderIdentities: [
        senderRow({ id: "s-self", address: "ops@operator.example", accountId: "self" }),
        senderRow({ id: "s-org", address: "news@client.example", accountId: "org" }),
      ],
    });
    h.connection = conn;
    function Page() {
      const { requests, navigation } = useEmailRequests(() => {});
      const feeds = useEmailFeeds();
      const recordFeeds = useEmailRecordFeeds(feeds);
      const writes = useCampaignWrites();
      const [settings, setSettings] = useState(DEFAULT_EMAIL_SETTINGS);
      return (
        <EmailNavigationContext.Provider value={navigation}>
          <AccountOptionsProvider>
            <button onClick={() => navigation.openSettings("org")}>Open Client</button>
            <SettingsSection
              feeds={feeds}
              recordFeeds={recordFeeds}
              writes={writes}
              settings={settings}
              update={(patch) => setSettings((held) => ({ ...held, ...patch }))}
              request={requests.settings}
              onOrganization={() => {}}
            />
          </AccountOptionsProvider>
        </EmailNavigationContext.Provider>
      );
    }
    render(withSession(<Page />));

    expect(await screen.findByText("ops@operator.example")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open Client" }));
    expect(await screen.findByText("news@client.example")).toBeTruthy();
    expect(screen.queryByText("ops@operator.example")).toBeNull();

    // The person goes back to the operator's own; asking again brings them to Client again.
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Operator organization");
    expect(await screen.findByText("ops@operator.example")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open Client" }));
    expect(await screen.findByText("news@client.example")).toBeTruthy();
  });

  it("keeps preferences as switches in this browser", async () => {
    const { store } = mount(fakeConnection());

    const archived = (await screen.findByRole("switch", { name: "Include archived" })) as HTMLInputElement;
    const tracking = screen.getByRole("switch", { name: "Track opens and clicks" }) as HTMLInputElement;
    expect(archived.checked).toBe(false);
    expect(tracking.checked).toBe(true);

    fireEvent.click(archived);
    expect(store.load().includeArchived).toBe(true);
    fireEvent.click(tracking);
    expect(store.load().trackByDefault).toBe(false);
    fireEvent.click(archived);
    expect(store.load().includeArchived).toBe(false);

    // The one preference that changes a campaign says that it changes none that exist.
    expect(screen.getByText(/Existing campaigns stay unchanged/)).toBeTruthy();
    // Each preference is named once: no second control, no second caption for it.
    expect(screen.getAllByRole("switch")).toHaveLength(2);
    expect(screen.getAllByRole("combobox", { name: "Opening tab" })).toHaveLength(1);
    expect(screen.queryByText("Show archived items")).toBeNull();

    chooseOption(screen.getByRole("combobox", { name: "Opening tab" }), "Audiences");
    expect(store.load().defaultSection).toBe("audiences");
  });

  it("offers an owner the tabs they can open, and a writer the same without Inbox, never Logs or Settings", async () => {
    mount(fakeConnection());
    const owner = openSelect(await screen.findByRole("combobox", { name: "Opening tab" }));
    expect(within(owner).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "Overview",
      "Campaigns",
      "Automations",
      "Audiences",
      "Templates",
      "Inbox",
    ]);
    cleanup();

    installSeededAccess("writer");
    mount(fakeConnection(), { role: "writer" });
    const writer = openSelect(await screen.findByRole("combobox", { name: "Opening tab" }));
    const names = within(writer).getAllByRole("option").map((option) => option.textContent);
    expect(names).toEqual(["Overview", "Campaigns", "Automations", "Audiences", "Templates"]);
    expect(names).not.toContain("Logs");
    expect(names).not.toContain("Settings");
  });

  it("opens a window on the preferred tab, unless the person cannot open that tab", async () => {
    const open = (role: string | undefined, navigate: (sectionId: string) => void) => {
      h.connection = fakeConnection();
      return render(
        withSession(
          <EmailApp sectionId="overview" navigate={navigate} askContext={() => {}} store={memoryStore({ defaultSection: "inbox" })} uploads={{ upload: vi.fn() }} />,
          { role },
        ),
      );
    };

    const owner = vi.fn<(sectionId: string) => void>();
    open(undefined, owner);
    await waitFor(() => expect(owner).toHaveBeenCalledWith("inbox"));
    cleanup();

    // The preference is the browser's, and a browser is shared: a writer on it
    // is not sent to a tab they cannot open.
    installSeededAccess("writer");
    const writer = vi.fn<(sectionId: string) => void>();
    open("writer", writer);
    await screen.findByRole("heading", { level: 3, name: "Overview" });
    await act(async () => {});
    expect(writer).not.toHaveBeenCalled();
  });

  it("leads with the Set up group for people who can configure", async () => {
    const unconfigured = readiness(true, [verdict("email", "unconfigured"), verdict("campaigns", "configured")]);
    mount(fakeConnection(), { readiness: unconfigured });

    const group = await screen.findByRole("region", { name: "Set up Email" });
    const head = screen.getByRole("heading", { level: 3, name: "Settings" });
    const sending = screen.getByRole("heading", { level: 4, name: "Sending domain" });
    // Under the title, over the subjects.
    expect(head.compareDocumentPosition(group) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(group.compareDocumentPosition(sending) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    cleanup();

    installSeededAccess("writer");
    mount(fakeConnection(), { role: "writer", readiness: unconfigured });
    await screen.findByRole("heading", { level: 3, name: "Settings" });
    expect(screen.queryByRole("region", { name: "Set up Email" })).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Sending domain
// ---------------------------------------------------------------------------

describe("Sending domain", () => {
  it("offers domain setup only to owners and developers", async () => {
    const owner = fakeConnection();
    mount(owner);
    expect(await screen.findByRole("button", { name: "Add sending domain" })).toBeTruthy();
    cleanup();

    installSeededAccess("writer");
    const writer = fakeConnection();
    mount(writer, { role: "writer" });
    expect(await screen.findByText("An owner or developer sets up sending domains.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add sending domain" })).toBeNull();
    // Nothing is asked of the cluster that it would only refuse, and a tool for
    // owners and developers is not offered to somebody it would refuse.
    expect(writer.query.emailAzureSetup).not.toHaveBeenCalled();
    expect(writer.query.packageCampaigns).not.toHaveBeenCalled();
    expect(screen.queryByRole("heading", { level: 4, name: "Package" })).toBeNull();
  });

  it("says cluster email is not set up, instead of offering a domain it cannot add", async () => {
    const conn = fakeConnection({ azure: () => ({ status: "unconfigured", capture: false }) });
    mount(conn);
    expect(await screen.findByText("Cluster email isn't set up.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add sending domain" })).toBeNull();
  });

  it("draws the organization's domain, and the wizard and page for it take the pane and give it back", async () => {
    const plan = { domain: "client.example", subscriptionId: "s", resourceGroup: "mail", emailService: "e", communicationService: "c", dataLocation: "United States" };
    const conn = fakeConnection({
      azure: (args) =>
        args.action === "clusterStatus"
          ? { status: "connected", resourceGroup: "mail", dataLocation: "United States" }
          : { status: "ready", sender: "news@client.example", replyTo: "hello@client.example", plan },
    });
    mount(conn);

    const row = await screen.findByRole("button", { name: "Manage email for Operator organization" });
    expect(within(row).getByText("Ready")).toBeTruthy();
    fireEvent.click(row);

    // The domain's page replaces the list rather than standing beneath it.
    expect(await screen.findByRole("heading", { level: 3, name: "client.example" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Settings" })).toBeNull();
    expect(screen.queryByRole("heading", { level: 4, name: "Senders" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Back to Settings" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    expect(screen.getByRole("heading", { level: 4, name: "Senders" })).toBeTruthy();
  });

  it("opens the wizard for a new domain over the page, and Leave brings the page back", async () => {
    mount(fakeConnection());
    fireEvent.click(await screen.findByRole("button", { name: "Add sending domain" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Add sending domain" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 4, name: "Senders" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Opening tab" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Leave" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Opening tab" })).toBeTruthy();
  });
});

describe("Cluster email notice", () => {
  const show = (role: string, status = "unconfigured") => {
    installSeededAccess(role);
    h.connection = fakeConnection();
    render(withSession(withOs(<ClusterEmailNotice cluster={{ status }} />, role), { role }));
  };

  it("takes an owner or developer to Settings, and tells anybody else what is wrong without a button that goes nowhere", () => {
    for (const role of ["owner", "developer"]) {
      show(role);
      expect(screen.getByText("Cluster email isn't set up.")).toBeTruthy();
      expect(screen.getByRole("button", { name: "Open email settings" })).toBeTruthy();
      cleanup();
    }
    for (const role of ["admin", "writer"]) {
      show(role);
      expect(screen.getByText("Cluster email isn't set up.")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Open email settings" })).toBeNull();
      cleanup();
    }
  });

  it("says a connection that needs signing in again is that, not that it was never set up", () => {
    show("owner", "reauthorize");
    expect(screen.getByText("Cluster email needs signing in again.")).toBeTruthy();
    expect(screen.queryByText("Cluster email isn't set up.")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Senders
// ---------------------------------------------------------------------------

describe("Senders", () => {
  /** The list's rows, top to bottom: the address and the one word of state. */
  const sendersOf = () =>
    within(screen.getByRole("list", { name: "Senders" }))
      .getAllByRole("button")
      .map((row) => ({ address: row.querySelector(".os-row-name")?.textContent, state: row.querySelector(".os-record-status")?.textContent }));

  it("lists the organization's senders newest first, and archived ones only with Include archived", async () => {
    const seed = {
      senderIdentities: [
        senderRow({ id: "old", address: "old@acme.com", createdAt: "2026-07-01T00:00:00Z" }),
        senderRow({ id: "new", address: "new@acme.com", createdAt: "2026-09-01T00:00:00Z" }),
        senderRow({ id: "gone", address: "gone@acme.com", status: "disabled", createdAt: "2026-06-01T00:00:00Z" }),
        senderRow({ id: "elsewhere", address: "else@other.com", accountId: "other", createdAt: "2026-10-01T00:00:00Z" }),
      ],
    };
    mount(fakeConnection(seed));
    expect(await screen.findByText("new@acme.com")).toBeTruthy();
    expect(sendersOf()).toEqual([
      { address: "new@acme.com", state: "Active" },
      { address: "old@acme.com", state: "Active" },
    ]);
    expect(screen.queryByText("gone@acme.com")).toBeNull();
    expect(screen.queryByText("else@other.com")).toBeNull();
    cleanup();

    mount(fakeConnection(seed), { settings: { includeArchived: true } });
    expect(await screen.findByText("gone@acme.com")).toBeTruthy();
    expect(sendersOf()).toEqual([
      { address: "new@acme.com", state: "Active" },
      { address: "old@acme.com", state: "Active" },
      { address: "gone@acme.com", state: "Archived" },
    ]);
  });

  it("puts a sender that arrives later first", async () => {
    const conn = fakeConnection({ senderIdentities: [senderRow({ id: "first", address: "first@acme.com", createdAt: "2026-07-01T00:00:00Z" })] });
    mount(conn);
    await screen.findByText("first@acme.com");
    act(() => conn.subscriptions.emit(SENDER, senderRow({ id: "later", address: "later@acme.com", createdAt: "2026-10-05T00:00:00Z" })));
    expect(await screen.findByText("later@acme.com")).toBeTruthy();
    expect(sendersOf().map((row) => row.address)).toEqual(["later@acme.com", "first@acme.com"]);
  });

  it("says what no senders means for the cluster's own organization and for a client", async () => {
    mount(fakeConnection());
    expect(await screen.findByText("No senders yet")).toBeTruthy();
    expect(screen.getByText(/sends from the cluster's sender/)).toBeTruthy();
    cleanup();

    mount(fakeConnection({ accounts: [{ id: "org", name: "Client", status: "active" }] }), {});
    expect(await screen.findByText("No senders yet")).toBeTruthy();
    expect(screen.queryByText(/sends from the cluster's sender/)).toBeNull();
    expect(screen.getByText(/Add one before this organization sends mail/)).toBeTruthy();
  });

  it("does not call a failed read an empty list", async () => {
    const conn = fakeConnection();
    conn.query.senderIdentities.mockRejectedValue(new Error("Senders are not yours to read."));
    mount(conn);
    expect(await screen.findByText("Senders are not yours to read.")).toBeTruthy();
    expect(screen.queryByText("No senders yet")).toBeNull();
  });

  it("shows a skeleton, not a notice, while the senders are still being read", async () => {
    const conn = fakeConnection();
    conn.query.senderIdentities.mockImplementation(() => new Promise(() => {}));
    const { view } = mount(conn);
    await screen.findByRole("heading", { level: 4, name: "Senders" });
    expect(view.container.querySelector(".os-record-skeleton")).not.toBeNull();
    expect(screen.queryByText("No senders yet")).toBeNull();
    expect(view.container.querySelector(".os-notice")).toBeNull();
  });
});

describe("Add a sender", () => {
  async function openWizard() {
    fireEvent.click(await screen.findByRole("button", { name: "Add a sender" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Add a sender" })).toBeTruthy();
  }
  const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
  const press = (name: string) => fireEvent.click(screen.getByRole("button", { name }));

  it("adds a sender through the wizard", async () => {
    const conn = fakeConnection();
    mount(conn);
    await openWizard();

    // Nothing to go forward with until the address is answered.
    expect(screen.queryByRole("button", { name: "Next" })).toBeNull();
    type("Address", "news@acme.com");
    press("Next");
    type("Display name", "Acme News");
    press("Next");
    // Reply to is the one optional question: it can be skipped.
    press("Skip");
    expect(await screen.findByText("Send as Acme News <news@acme.com>.")).toBeTruthy();
    // One word for the thing being added (Ruling 6), in what Review says about it too.
    expect(screen.queryByText(/mailbox/i)).toBeNull();
    expect(conn.query.createSenderIdentity).not.toHaveBeenCalled();

    // Two presses before the page has drawn the first one's busy state: the
    // second is the same press, not a second sender.
    const add = screen.getByRole("button", { name: "Add sender" });
    act(() => {
      fireEvent.click(add);
      fireEvent.click(add);
    });
    await waitFor(() => expect(conn.query.createSenderIdentity).toHaveBeenCalledTimes(1));
    expect(conn.query.createSenderIdentity).toHaveBeenCalledWith({
      senderIdentityId: expect.stringMatching(/\S+/),
      address: "news@acme.com",
      fromName: "Acme News",
      accountId: "self",
    });
    // Skipped means not sent, rather than sent empty.
    expect(conn.query.createSenderIdentity.mock.calls[0]![0].replyTo ?? "").toBe("");
  });

  it("keeps a reply to that was typed on Next, and lets go of it on Skip", async () => {
    const conn = fakeConnection();
    mount(conn);
    await openWizard();
    type("Address", "news@acme.com");
    press("Next");
    type("Display name", "Acme News");
    press("Next");

    type("Reply to", "help@acme.com");
    press("Skip");
    expect(await screen.findByText("Send as Acme News <news@acme.com>.")).toBeTruthy();

    // Back to the question: what Skip let go of is gone, and the answer reads as skipped.
    fireEvent.click(screen.getByRole("button", { name: /Reply to/ }));
    expect((screen.getByLabelText("Reply to") as HTMLInputElement).value).toBe("");
    press("Next");
    expect(await screen.findByText("Send as Acme News <news@acme.com>.")).toBeTruthy();
    expect(conn.query.createSenderIdentity).not.toHaveBeenCalled();
  });

  it("keeps the same id when the add is refused and tried again, and shows the refusal verbatim", async () => {
    const conn = fakeConnection();
    conn.query.createSenderIdentity.mockRejectedValueOnce(new Error("address is not a valid mailbox"));
    mount(conn);
    await openWizard();
    type("Address", "news@acme.com");
    press("Next");
    type("Display name", "Acme News");
    press("Next");
    type("Reply to", "help@acme.com");
    press("Next");
    expect(await screen.findByText("Send as Acme News <news@acme.com>, replies to help@acme.com.")).toBeTruthy();

    press("Add sender");
    expect(await screen.findByText("address is not a valid mailbox")).toBeTruthy();
    // The wizard is still there with everything that was typed.
    expect(screen.getByText("Send as Acme News <news@acme.com>, replies to help@acme.com.")).toBeTruthy();

    press("Add sender");
    await waitFor(() => expect(conn.query.createSenderIdentity).toHaveBeenCalledTimes(2));
    const [first, second] = conn.query.createSenderIdentity.mock.calls.map(([args]) => args);
    expect(first!.senderIdentityId).toBe(second!.senderIdentityId);
    expect(second).toMatchObject({ replyTo: "help@acme.com" });
  });

  it("opens the new sender's page when its row arrives, and Cancel leaves nothing written", async () => {
    const conn = fakeConnection();
    mount(conn);
    await openWizard();
    press("Cancel");
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    expect(conn.query.createSenderIdentity).not.toHaveBeenCalled();

    await openWizard();
    type("Address", "news@acme.com");
    press("Next");
    type("Display name", "Acme News");
    press("Next");
    press("Skip");
    press("Add sender");
    await waitFor(() => expect(conn.query.createSenderIdentity).toHaveBeenCalledTimes(1));
    const id = conn.query.createSenderIdentity.mock.calls[0]![0].senderIdentityId as string;

    // Nothing is drawn locally: the page opens when the live feed delivers the row.
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    act(() => conn.subscriptions.emit(SENDER, senderRow({ id, address: "news@acme.com", fromName: "Acme News" })));
    expect(await screen.findByRole("heading", { level: 3, name: "news@acme.com" })).toBeTruthy();
  });

  it("asks which organization only when there is a choice, starting from the page's", async () => {
    const conn = fakeConnection({ accounts: TWO_ORGANIZATIONS });
    mount(conn);
    chooseOption(await screen.findByRole("combobox", { name: "Organization" }), "Client");
    await openWizard();

    // The question is already answered by the page, and can be answered again.
    const rail = screen.getByRole("list", { name: "Adding a sender" });
    expect(within(rail).getByText("Organization")).toBeTruthy();
    expect(within(rail).getByText("Client")).toBeTruthy();
    type("Address", "news@client.example");
    press("Next");
    type("Display name", "Client News");
    press("Next");
    press("Skip");
    press("Add sender");
    await waitFor(() => expect(conn.query.createSenderIdentity).toHaveBeenCalledTimes(1));
    expect(conn.query.createSenderIdentity.mock.calls[0]![0]).toMatchObject({ accountId: "org" });
  });
});

describe("A sender's page", () => {
  const sender = senderRow({
    id: "s1",
    address: "news@acme.com",
    fromName: "Acme News",
    replyTo: "help@acme.com",
    notes: "Warmed up in June",
  });

  async function openSender(conn = fakeConnection({ senderIdentities: [sender] })) {
    mount(conn);
    fireEvent.click(await screen.findByText("news@acme.com"));
    expect(await screen.findByRole("heading", { level: 3, name: "news@acme.com" })).toBeTruthy();
    return conn;
  }

  it("carries one bar, and what changes the sender is only in it", async () => {
    const conn = await openSender();

    // The list is parked behind this page, so the facts are read from the page's own.
    const fact = (label: string) => screen.getByText(label, { selector: "dt" }).nextElementSibling?.textContent;
    expect(fact("Organization")).toBe("Operator organization");
    expect(fact("Display name")).toBe("Acme News");
    expect(fact("Reply to")).toBe("help@acme.com");
    expect(fact("Notes")).toBe("Warmed up in June");
    expect(fact("Added")).toBeTruthy();
    expect(screen.getAllByRole("group", { name: "What you can do with this" })).toHaveLength(1);
    expect(barButtons()).toEqual(["Archive", "Edit"]);
    // There is no delete anywhere, and nothing that cannot work is drawn.
    expect(screen.queryByText(/^Delete/)).toBeNull();
    expect(deadButtons()).toEqual([]);

    // Archive asks first, and says what it costs.
    fireEvent.click(within(bar()).getByRole("button", { name: "Archive" }));
    expect(
      within(bar()).getByText("Archive news@acme.com? Campaigns and automations that send as it stop until it's restored."),
    ).toBeTruthy();
    expect(barButtons()).toEqual(["Keep", "Archive"]);
    expect(conn.query.setSenderIdentityStatus).not.toHaveBeenCalled();

    fireEvent.click(within(bar()).getByRole("button", { name: "Keep" }));
    expect(barButtons()).toEqual(["Archive", "Edit"]);
    expect(conn.query.setSenderIdentityStatus).not.toHaveBeenCalled();

    fireEvent.click(within(bar()).getByRole("button", { name: "Archive" }));
    fireEvent.click(within(bar()).getByRole("button", { name: "Archive" }));
    await waitFor(() =>
      expect(conn.query.setSenderIdentityStatus).toHaveBeenCalledWith({ senderIdentityId: "s1", status: "disabled" }),
    );

    // The page stays, and now reads the state the live feed delivered.
    act(() => conn.subscriptions.emit(SENDER, { ...sender, status: "disabled" }));
    await waitFor(() => expect(barButtons()).toEqual(["Restore"]));
    expect(within(bar()).getByText("Archived")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 3, name: "news@acme.com" })).toBeTruthy();
    expect(deadButtons()).toEqual([]);

    fireEvent.click(within(bar()).getByRole("button", { name: "Restore" }));
    await waitFor(() =>
      expect(conn.query.setSenderIdentityStatus).toHaveBeenLastCalledWith({ senderIdentityId: "s1", status: "active" }),
    );
  });

  it("shows the engine's sentence beside the bar when archiving is refused", async () => {
    const conn = fakeConnection({ senderIdentities: [sender] });
    conn.query.setSenderIdentityStatus.mockRejectedValueOnce(new Error("Only the owner of this sender can archive it."));
    await openSender(conn);
    fireEvent.click(within(bar()).getByRole("button", { name: "Archive" }));
    fireEvent.click(within(bar()).getByRole("button", { name: "Archive" }));
    expect(await screen.findByText("Only the owner of this sender can archive it.")).toBeTruthy();
    // Still the sender it was, still asking.
    expect(barButtons()).toEqual(["Keep", "Archive"]);
  });

  it("edits the display name, reply to and notes, and can clear a reply to", async () => {
    const conn = await openSender();
    fireEvent.click(within(bar()).getByRole("button", { name: "Edit" }));
    expect(barButtons()).toEqual(["Cancel"]);

    // Nothing to save until something changed, and the address is not editable.
    expect(screen.queryByLabelText("Address")).toBeNull();
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Acme Team" } });
    fireEvent.change(screen.getByLabelText("Reply to"), { target: { value: "" } });
    expect(barButtons()).toEqual(["Cancel", "Save"]);

    // A blank display name cannot be saved: Save is not offered rather than refused.
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: " " } });
    expect(barButtons()).toEqual(["Cancel"]);
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Acme Team" } });

    fireEvent.click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(conn.query.updateSenderIdentity).toHaveBeenCalledTimes(1));
    // An explicit empty reply to clears it; leaving the field out would keep the old one.
    expect(conn.query.updateSenderIdentity).toHaveBeenCalledWith({
      senderIdentityId: "s1",
      address: "news@acme.com",
      fromName: "Acme Team",
      replyTo: "",
      accountId: "self",
      notes: "Warmed up in June",
    });
  });

  it("offers an archived sender only Restore", async () => {
    const archived = senderRow({ id: "s2", address: "old@acme.com", status: "disabled" });
    mount(fakeConnection({ senderIdentities: [archived] }), { settings: { includeArchived: true } });
    fireEvent.click(await screen.findByText("old@acme.com"));
    await screen.findByRole("heading", { level: 3, name: "old@acme.com" });
    expect(barButtons()).toEqual(["Restore"]);
  });

  it("leaves what was chosen on Settings standing while a page has the pane", async () => {
    // The page takes the pane over, but the page it left stays mounted, parked
    // the way a section is: a choice not yet saved survives the visit.
    const conn = fakeConnection({
      senderIdentities: [sender],
      audiences: [audienceRow({ id: "reviewers", name: "Reviewers" })],
    });
    mount(conn);
    chooseOption(await screen.findByRole("combobox", { name: "Testing audience" }), "Reviewers");
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();

    fireEvent.click(screen.getByText("news@acme.com"));
    await screen.findByRole("heading", { level: 3, name: "news@acme.com" });
    expect(screen.queryByRole("combobox", { name: "Testing audience" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Back to Settings" }));

    expect((await screen.findByRole("combobox", { name: "Testing audience" })).textContent).toContain("Reviewers");
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
    expect(conn.query.campaignConfigureTestAudience).not.toHaveBeenCalled();
  });

  it("goes back to Settings with the list where it was", async () => {
    await openSender();
    fireEvent.click(screen.getByRole("button", { name: "Back to Settings" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    expect(screen.getByText("news@acme.com")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Testing
// ---------------------------------------------------------------------------

describe("Testing", () => {
  const reviewers = audienceRow({ id: "reviewers", name: "Reviewers" });
  const others = audienceRow({ id: "others", name: "Other people" });
  const setting = { id: "ts", accountId: "self", audienceId: "reviewers", createdAt: "2026-09-30T00:00:00Z" };

  it("saves the testing audience only when it changes", async () => {
    const conn = fakeConnection({ audiences: [reviewers, others], testSettings: [setting] });
    mount(conn);

    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("Reviewers"));
    // Nothing differs from what is saved, so nothing is offered.
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();

    chooseOption(select, "Other people");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(conn.query.campaignConfigureTestAudience).toHaveBeenCalledWith({
        accountId: "self",
        audienceId: "others",
        expectedRevision: "2026-09-30T00:00:00Z",
      }),
    );

    // Choosing back what is saved takes the act away again.
    chooseOption(screen.getByRole("combobox", { name: "Testing audience" }), "Reviewers");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("offers only this organization's active audiences, and saving with no row yet sends no revision", async () => {
    const conn = fakeConnection({
      audiences: [reviewers, audienceRow({ id: "foreign", name: "Another client's", accountId: "other" }), audienceRow({ id: "retired", name: "Retired list", status: "archived" })],
    });
    mount(conn);
    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("None selected"));
    const list = openSelect(select);
    expect(within(list).getAllByRole("option").map((option) => option.textContent)).toEqual(["None selected", "Reviewers"]);
    fireEvent.click(within(list).getByRole("option", { name: "Reviewers" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(conn.query.campaignConfigureTestAudience).toHaveBeenCalledTimes(1));
    expect(conn.query.campaignConfigureTestAudience).toHaveBeenCalledWith({ accountId: "self", audienceId: "reviewers", expectedRevision: undefined });
  });

  it("renders a refusal verbatim and keeps the choice", async () => {
    const conn = fakeConnection({ audiences: [reviewers, others], testSettings: [setting] });
    conn.query.campaignConfigureTestAudience.mockRejectedValueOnce(new Error("testing audience changed; refresh settings before saving"));
    mount(conn);
    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("Reviewers"));
    chooseOption(select, "Other people");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("testing audience changed; refresh settings before saving")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Other people");
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
  });

  it("reads again only the organization that changed, and shows the saved choice when the feed delivers it", async () => {
    const conn = fakeConnection({
      accounts: TWO_ORGANIZATIONS,
      audiences: [reviewers, others, audienceRow({ id: "client", name: "Client list", accountId: "org" })],
      testSettings: [setting, { id: "ts2", accountId: "org", audienceId: "client", createdAt: "2026-09-01T00:00:00Z" }],
    });
    mount(conn);
    // Both organizations are in the feed once the page has visited them.
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Reviewers"));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Client list"));
    const readsBefore = conn.query.campaignTestSettings.mock.calls.length;

    chooseOption(screen.getByRole("combobox", { name: "Testing audience" }), "None selected");
    conn.query.campaignTestSettings.mockImplementation(async (args) =>
      rowsResult(
        (args as { accountId?: string }).accountId === "org"
          ? [{ id: "ts2", accountId: "org", audienceId: "", createdAt: "2026-10-01T00:00:00Z" }]
          : [setting],
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(conn.query.campaignConfigureTestAudience).toHaveBeenCalledTimes(1));

    // One more read, and it is for the organization that was saved.
    await waitFor(() => expect(conn.query.campaignTestSettings.mock.calls.length).toBe(readsBefore + 1));
    expect(conn.query.campaignTestSettings.mock.calls.at(-1)![0]).toEqual({ accountId: "org" });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Save" })).toBeNull());
    expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("None selected");
  });

  it("shows a skeleton, never a warning, while the testing audience is still being read", async () => {
    const conn = fakeConnection({ audiences: [reviewers], testSettings: [setting] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    const { view } = mount(conn);

    await screen.findByRole("heading", { level: 4, name: "Testing" });
    expect(view.container.querySelector(".os-content-skeleton")).not.toBeNull();
    expect(screen.queryByText("Testing settings are not confirmed.")).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Testing audience" })).toBeNull();

    await act(async () => { release(); });
    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("Reviewers"));
    expect(view.container.querySelector(".os-content-skeleton")).toBeNull();
  });

  it("says which organization's testing audience was refused, in the engine's sentence, and can try again", async () => {
    const conn = fakeConnection({ audiences: [reviewers], testSettings: [setting] });
    conn.query.campaignTestSettings.mockRejectedValueOnce(new Error("Reading testing settings is not yours to do."));
    mount(conn);
    expect(await screen.findByText("Reading testing settings is not yours to do.")).toBeTruthy();
    // A refusal is not an empty choice: no Select to save a guess through.
    expect(screen.queryByRole("combobox", { name: "Testing audience" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("Reviewers"));
  });

  it("offers nothing to save over a testing audience when the audiences could not be read", async () => {
    // The saved choice names an audience that exists, and the list it would be
    // chosen from was refused. A list that came back empty is a different thing
    // from one that was never read: drawn as the first, the Select calls the
    // saved audience "Unavailable" and offers to clear it.
    const conn = fakeConnection({ testSettings: [setting] });
    conn.query.audiences.mockRejectedValue(new Error("Audiences are not yours to read."));
    mount(conn);

    expect(await screen.findByText("Audiences are not yours to read.")).toBeTruthy();
    const again = screen.getByRole("button", { name: "Try again" });
    expect(screen.queryByRole("combobox", { name: "Testing audience" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByText("Unavailable audience")).toBeNull();
    expect(conn.query.campaignConfigureTestAudience).not.toHaveBeenCalled();

    // Reading them again brings the choice back, as it was saved.
    conn.query.audiences.mockImplementation(async () => rowsResult([reviewers]));
    fireEvent.click(again);
    const select = await screen.findByRole("combobox", { name: "Testing audience" });
    await waitFor(() => expect(select.textContent).toContain("Reviewers"));
    expect(screen.queryByText("Audiences are not yours to read.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("says both when the testing settings and the audiences were refused", async () => {
    const conn = fakeConnection({ testSettings: [setting] });
    conn.query.audiences.mockRejectedValue(new Error("Audiences are not yours to read."));
    conn.query.campaignTestSettings.mockRejectedValue(new Error("Reading testing settings is not yours to do."));
    mount(conn);
    expect(await screen.findByText("Audiences are not yours to read.")).toBeTruthy();
    expect(await screen.findByText("Reading testing settings is not yours to do.")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Try again" })).toHaveLength(2);
    expect(screen.queryByRole("combobox", { name: "Testing audience" })).toBeNull();
  });

  it("keeps the feed's other organizations when one is refused", async () => {
    const conn = fakeConnection({
      accounts: TWO_ORGANIZATIONS,
      audiences: [reviewers, audienceRow({ id: "client", name: "Client list", accountId: "org" })],
      testSettings: [setting, { id: "ts2", accountId: "org", audienceId: "client", createdAt: "2026-09-01T00:00:00Z" }],
    });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => {
      if ((args as { accountId?: string }).accountId === "org") throw new Error("Client testing settings are not yours to read.");
      return answer(args, opts);
    });
    mount(conn);
    // The operator's own organization reads fine...
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Reviewers"));
    // ...and the refused one says so for itself alone.
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    expect(await screen.findByText("Client testing settings are not yours to read.")).toBeTruthy();
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Operator organization");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Reviewers"));
    expect(screen.queryByText("Client testing settings are not yours to read.")).toBeNull();
  });

  it("reads an organization with no mail only once the page asks for it, over the one subscription", async () => {
    const conn = fakeConnection({
      accounts: TWO_ORGANIZATIONS,
      testSettings: [{ id: "ts2", accountId: "org", audienceId: "elsewhere", createdAt: "2026-09-01T00:00:00Z" }],
    });
    mount(conn);
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("None selected"));
    const asked = () => conn.query.campaignTestSettings.mock.calls.map(([args]) => (args as { accountId: string }).accountId);
    // The operator's organization has no campaign or audience either, and is
    // the one the page is on, so it is the one asked.
    expect(asked()).toEqual(["self"]);

    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    // A saved audience this person's list does not hold is said, not dropped.
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Testing audience" }).textContent).toContain("Unavailable audience"));
    expect(asked()).toContain("org");
    expect(conn.subscriptions.subscribeGraph.mock.calls.filter(([, o]) => o.concept === TEST_SETTINGS)).toHaveLength(1);
    expect(peakSubscriptions(conn).get(TEST_SETTINGS)).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Package
// ---------------------------------------------------------------------------

describe("Package", () => {
  it("is for the chosen organization, and its acts sit on the panel's right edge", async () => {
    const conn = fakeConnection({ accounts: TWO_ORGANIZATIONS });
    mount(conn);
    await screen.findByRole("heading", { level: 4, name: "Package" });
    await waitFor(() => expect(conn.query.packageCampaigns).toHaveBeenCalledWith(expect.objectContaining({ action: "installed", accountId: "self" }), expect.anything()));

    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await waitFor(() => expect(conn.query.packageCampaigns).toHaveBeenCalledWith(expect.objectContaining({ action: "installed", accountId: "org" }), expect.anything()));

    const load = screen.getByRole("button", { name: "Load package" });
    const exportIt = screen.getByRole("button", { name: "Export configuration" });
    expect(load.parentElement).toBe(exportIt.parentElement);
    expect(load.parentElement?.classList.contains("os-panel-actions")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Acts that cannot run are absent
// ---------------------------------------------------------------------------

describe("Package, while it is working", () => {
  it("shows the act that is running as busy, and disables nothing", async () => {
    const conn = fakeConnection();
    // The first read of the installed package never answers.
    conn.query.packageCampaigns.mockImplementation(() => new Promise(() => {}));
    mount(conn);
    await screen.findByRole("heading", { level: 4, name: "Package" });

    for (const name of ["Load package", "Export configuration"]) {
      expect(screen.getByRole("button", { name }).getAttribute("aria-busy")).toBe("true");
    }
    expect(deadButtons()).toEqual([]);
  });
});

describe("Settings draws no dead control", () => {
  it("has no disabled button on the page, in a wizard, or on a sender's page", async () => {
    const conn = fakeConnection({ senderIdentities: [senderRow({ id: "s1", address: "news@acme.com" })] });
    mount(conn);
    await screen.findByText("news@acme.com");
    await screen.findByRole("combobox", { name: "Testing audience" });
    expect(deadButtons()).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Add a sender" }));
    await screen.findByRole("heading", { level: 3, name: "Add a sender" });
    expect(deadButtons()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(await screen.findByText("news@acme.com"));
    await screen.findByRole("heading", { level: 3, name: "news@acme.com" });
    expect(deadButtons()).toEqual([]);
  });
});
