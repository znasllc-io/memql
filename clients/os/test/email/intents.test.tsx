import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { OS_REGISTRY } = await import("../../src/apps/registry");
const { EmailApp, useOpenRecord, useOpenSettings } = await import("../../src/apps/email/EmailApp");
const { EmailNavigationContext, recordNamed, useEmailRequests, useOpenInbox } = await import("../../src/apps/email/openRequest");
const { LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const { AudiencesSection } = await import("../../src/apps/email/audiences/AudiencesSection");
const { CampaignsSection } = await import("../../src/apps/email/campaigns/CampaignsSection");
const { AutomationsSection } = await import("../../src/apps/email/automations/AutomationsSection");
const { TemplatesSection } = await import("../../src/apps/email/templates/TemplatesSection");
const { useCampaignWrites } = await import("../../src/apps/email/actions");
const { campaignFromRow } = await import("../../src/apps/email/rows");
const { useEmailFeeds, useEmailRecordFeeds } = await import("../../src/apps/email/useEmail");
const {
  audienceRow,
  campaignRow,
  fakeConnection,
  rowsResult,
  ruleRow,
  templateRow,
  withSession,
} = await import("./harness");

import type { OpenRequest } from "../../src/apps/email/EmailApp";
import type { OpenSection } from "../../src/apps/email/openRequest";
import type { FakeSeed } from "./harness";

type Conn = ReturnType<typeof fakeConnection>;
type Intent = { id: string; payload: Record<string, unknown> };

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

/** An upload provider that never touches the network. */
function fakeUploads() {
  return { upload: vi.fn() };
}

/**
 * One Email window as the shell holds it: it remembers the section the app was
 * last sent to, and re-renders the app on it. `navigate` is a spy that ALSO
 * moves the window, so "the record's page is on screen" is a thing to assert.
 *
 * `consumeIntent` is a spy that, unlike the shell's, leaves the intent standing
 * in the props -- the shell clears it a render later, and a handler that acted
 * on a standing intent twice is exactly the bug that gap would hide.
 */
function mountWindow(
  connection: Conn,
  { section = "overview", settings = {}, intent }: { section?: string; settings?: Record<string, unknown>; intent?: Intent } = {},
) {
  h.connection = connection;
  const navigate = vi.fn();
  const consumeIntent = vi.fn();
  const store = memoryStore(settings);
  const uploads = fakeUploads();
  function Window({ standing }: { standing?: Intent }) {
    const [current, setCurrent] = useState(section);
    return withSession(
      <EmailApp
        sectionId={current}
        navigate={(id, options) => {
          navigate(id, options);
          setCurrent(id);
        }}
        askContext={() => {}}
        intent={standing}
        consumeIntent={consumeIntent}
        store={store}
        uploads={uploads}
      />,
    );
  }
  const view = render(<Window standing={intent} />);
  return {
    view,
    navigate,
    consumeIntent,
    /** Hand the window an instruction, as `openApp` does. */
    deliver: (next?: Intent) => view.rerender(<Window standing={next} />),
  };
}

/** One retained section's own box. */
function pane(id: string): HTMLElement {
  const node = document.querySelector<HTMLElement>(`[data-email-section="${id}"]`);
  if (node === null) throw new Error(`the ${id} section is not mounted`);
  return node;
}

const heading = (name: string) => screen.findByRole("heading", { level: 3, name });

// ---------------------------------------------------------------------------
// From search and Ask
// ---------------------------------------------------------------------------

describe("a record named by an intent", () => {
  it.each([
    ["campaignId", "campaigns", "August update"],
    ["audienceId", "audiences", "Newsletter"],
    ["templateId", "templates", "August copy"],
  ] as const)("an intent naming %s opens that page while another tab is showing", async (key, section, name) => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "x1" })],
      audiences: [audienceRow({ id: "x1" })],
      templates: [templateRow({ id: "x1" })],
    });
    const { navigate, consumeIntent, deliver } = mountWindow(conn);
    expect(pane("overview").hidden).toBe(false);

    deliver({ id: "i1", payload: { [key]: "x1" } });

    // The record's own page, with its way back to the list it came from.
    expect(await heading(name)).toBeTruthy();
    expect(screen.getByRole("button", { name: `Back to ${section[0]!.toUpperCase()}${section.slice(1)}` })).toBeTruthy();
    expect(navigate).toHaveBeenCalledTimes(1);
    expect(navigate).toHaveBeenCalledWith(section, { fromContent: true });
    expect(pane(section).hidden).toBe(false);
    expect(pane("overview").hidden).toBe(true);
    expect(consumeIntent).toHaveBeenCalledTimes(1);
    expect(consumeIntent).toHaveBeenCalledWith("i1");
  });

  it.each([
    ["a key it does not know", { colour: "red" }],
    ["an empty id", { campaignId: "" }],
    ["an id that is not text", { audienceId: 7 }],
  ] as [string, Record<string, unknown>][])("consumes a payload naming nothing known (%s), and opens nothing", async (_why, payload) => {
    const { navigate, consumeIntent, deliver } = mountWindow(fakeConnection());

    deliver({ id: "i2", payload });

    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i2"));
    expect(consumeIntent).toHaveBeenCalledTimes(1);
    expect(navigate).not.toHaveBeenCalled();
    expect(pane("overview").hidden).toBe(false);
  });

  it("acts on an instruction once, however often the window renders while it stands", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "x1" })] });
    const { navigate, consumeIntent, deliver } = mountWindow(conn);
    const intent = { id: "i1", payload: { campaignId: "x1" } };

    deliver(intent);
    await heading("August update");
    deliver(intent);
    deliver({ ...intent });

    expect(navigate).toHaveBeenCalledTimes(1);
    expect(consumeIntent).toHaveBeenCalledTimes(1);
  });

  it("opens a record the list hides: a finished campaign, an archived audience or template", async () => {
    // A finished campaign may be outside what the list is asking (Refine), and
    // the audiences and templates lists leave archived ones out unless Settings
    // says otherwise; search and Ask still reach every one, and an opening that
    // stopped at the list would answer a question about last year's send with
    // this year's.
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "done", name: "Last year", status: "sent" })],
      audiences: [audienceRow({ id: "old", name: "Old list", status: "archived" })],
      templates: [templateRow({ id: "old", name: "Old copy", status: "archived" })],
    });
    const { deliver } = mountWindow(conn);

    deliver({ id: "i1", payload: { campaignId: "done" } });
    expect(await heading("Last year")).toBeTruthy();

    deliver({ id: "i2", payload: { audienceId: "old" } });
    expect(await heading("Old list")).toBeTruthy();

    deliver({ id: "i3", payload: { templateId: "old" } });
    expect(await heading("Old copy")).toBeTruthy();
  });

  it("shows the list while the record has not arrived, and opens it when it does", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const conn = fakeConnection();
    conn.query.campaigns.mockImplementation(async () => {
      await gate;
      return rowsResult([campaignRow({ id: "x1" })]);
    });
    const { navigate, consumeIntent, deliver } = mountWindow(conn);

    deliver({ id: "i1", payload: { campaignId: "x1" } });

    // Nothing to open yet, so the person is on the list and not on a blank page.
    expect(await heading("Campaigns")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "August update" })).toBeNull();
    // And nothing is said about it: the cluster has not answered, so the record is not "missing".
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
    expect(navigate).toHaveBeenCalledWith("campaigns", { fromContent: true });
    expect(consumeIntent).toHaveBeenCalledWith("i1");

    await act(async () => release());
    expect(await heading("August update")).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });

  it("leaves the person on the list when the cluster has no such record", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "other", name: "Somebody else's" })] });
    const { deliver, consumeIntent } = mountWindow(conn);

    deliver({ id: "i1", payload: { campaignId: "missing" } });

    expect(await screen.findByText("Somebody else's")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 3, name: "Campaigns" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Back to Campaigns" })).toBeNull();
    expect(consumeIntent).toHaveBeenCalledWith("i1");
    // ...and says so, in place, rather than leaving the person on a list with
    // nothing to show for what they asked.
    expect(await screen.findByText("That campaign isn't available.")).toBeTruthy();
  });

  it("says the campaign is not available, with no campaigns at all, above the empty list", async () => {
    const { deliver } = mountWindow(fakeConnection());

    deliver({ id: "i1", payload: { campaignId: "missing" } });

    expect(await screen.findByText("That campaign isn't available.")).toBeTruthy();
    expect(screen.getByText("No campaigns yet")).toBeTruthy();
  });

  it("takes the sentence back when the campaign arrives after all, and opens it", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "other", name: "Somebody else's" })] });
    const { deliver } = mountWindow(conn);
    deliver({ id: "i1", payload: { campaignId: "late" } });
    expect(await screen.findByText("That campaign isn't available.")).toBeTruthy();

    await act(async () => {
      conn.subscriptions.emit("v1:campaigns:campaign", campaignRow({ id: "late", name: "Came late" }), "NODE_CREATED");
    });

    expect(await heading("Came late")).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });

  it("takes the sentence back when another campaign is asked for", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "x1" })] });
    const { deliver } = mountWindow(conn);
    deliver({ id: "i1", payload: { campaignId: "missing" } });
    expect(await screen.findByText("That campaign isn't available.")).toBeTruthy();

    deliver({ id: "i2", payload: { campaignId: "x1" } });

    expect(await heading("August update")).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });

  it("takes the sentence back when the person starts adding a campaign instead", async () => {
    const { deliver } = mountWindow(fakeConnection({ campaigns: [campaignRow({ id: "other", name: "Somebody else's" })] }));
    deliver({ id: "i1", payload: { campaignId: "missing" } });
    expect(await screen.findByText("That campaign isn't available.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Add a campaign" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
    // ...and it does not come back when the wizard is left: that request was answered.
    fireEvent.click(screen.getByRole("button", { name: "Back to Campaigns" }));
    expect(await screen.findByText("Somebody else's")).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });

  it("does not call a campaign missing when the cluster could not be asked", async () => {
    // A refused read says nothing about whether the record exists, and the list
    // already says that it did not come back.
    const conn = fakeConnection();
    conn.query.campaigns.mockRejectedValue(new Error("read refused"));
    const { deliver } = mountWindow(conn);
    deliver({ id: "i1", payload: { campaignId: "x1" } });

    expect((await screen.findAllByText("This cluster did not return your campaigns.")).length).toBeGreaterThan(0);
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });

  it("opens the same record again when it is asked for again after going back", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "x1" })] });
    const { deliver, navigate, consumeIntent } = mountWindow(conn);

    deliver({ id: "i1", payload: { campaignId: "x1" } });
    await heading("August update");
    fireEvent.click(screen.getByRole("button", { name: "Back to Campaigns" }));
    expect(await heading("Campaigns")).toBeTruthy();

    // A new instruction, though the record is the same one.
    deliver({ id: "i2", payload: { campaignId: "x1" } });
    expect(await heading("August update")).toBeTruthy();
    expect(navigate).toHaveBeenCalledTimes(2);
    expect(consumeIntent.mock.calls.map(([id]) => id)).toEqual(["i1", "i2"]);
  });

  it("changes which record a section shows when another is asked for", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "a", name: "First" }), campaignRow({ id: "b", name: "Second" })],
    });
    const { deliver } = mountWindow(conn);

    deliver({ id: "i1", payload: { campaignId: "a" } });
    expect(await heading("First")).toBeTruthy();
    deliver({ id: "i2", payload: { campaignId: "b" } });
    expect(await heading("Second")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "First" })).toBeNull();
  });

  it("is not sent to the preferred tab when an instruction opened the window", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "x1" })] });
    const { navigate } = mountWindow(conn, {
      settings: { defaultSection: "templates" },
      intent: { id: "i1", payload: { campaignId: "x1" } },
    });

    expect(await heading("August update")).toBeTruthy();
    // The preference would have navigated after the instruction and won.
    expect(navigate.mock.calls).toEqual([["campaigns", { fromContent: true }]]);
  });

  it("still opens on the preferred tab when nothing asked for another", async () => {
    const { navigate } = mountWindow(fakeConnection(), { settings: { defaultSection: "templates" } });

    expect(await heading("Templates")).toBeTruthy();
    // A peer navigation, not one from content: there is nowhere it came from.
    expect(navigate).toHaveBeenCalledWith("templates", undefined);
  });

  it("is a contract with the manifest: every record destination it declares is one the app opens", async () => {
    const records = OS_REGISTRY.apps.find((app) => app.id === "email")?.records ?? [];
    expect(records.length).toBeGreaterThan(0);
    for (const record of records) {
      const conn = fakeConnection();
      const { navigate, consumeIntent, deliver, view } = mountWindow(conn);
      deliver({ id: "i1", payload: { [record.idField]: "x1" } });
      await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
      expect(navigate).toHaveBeenCalledWith(record.section, { fromContent: true });
      view.unmount();
    }
  });
});

// Logs takes an instruction of its own -- a subject to narrow to -- and the app
// consumes whatever it has nothing to open for. The order matters: if the app
// consumed first, the section would be handed nothing.
describe("an instruction that is another section's", () => {
  it("is taken by Logs first, and the app opens nothing for it", async () => {
    const { navigate, consumeIntent, deliver } = mountWindow(fakeConnection(), { section: "logs" });

    deliver({ id: "i1", payload: { subject: "campaign-1", subjectConcept: "v1:campaigns:campaign" } });

    expect(await screen.findByText("subject campaign-1")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
    expect(consumeIntent).toHaveBeenCalledWith("i1");
  });
});

describe("the record a page shows", () => {
  const rows = [campaignRow({ id: "a" }), campaignRow({ id: "b", status: "sent" }), campaignRow({ id: "c" })];

  it("is the one its id names in the whole feed, projected no further than it", () => {
    const project = vi.fn(campaignFromRow);
    expect(recordNamed(rows, project, "b")?.status).toBe("sent");
    expect(project).toHaveBeenCalledTimes(2);
    expect(recordNamed(rows, project, "missing")).toBeNull();
  });

  it("is nothing for no id, and reads nothing to say so", () => {
    const project = vi.fn(campaignFromRow);
    expect(recordNamed(rows, project, "")).toBeNull();
    expect(project).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// From inside the app
// ---------------------------------------------------------------------------

describe("opening a record from inside the app", () => {
  it("records a request for the section and goes there from content", () => {
    const navigate = vi.fn();
    const { result } = renderHook(() => useEmailRequests(navigate));
    expect(result.current.requests).toEqual({});

    act(() => result.current.navigation.openRecord("audiences", { add: true }));
    expect(navigate).toHaveBeenLastCalledWith("audiences", { fromContent: true });
    expect(result.current.requests).toEqual({ audiences: { add: true, revision: 1 } });

    // Every ask is a new revision, even for the record that was asked for last.
    act(() => result.current.navigation.openRecord("audiences", { id: "a1" }));
    act(() => result.current.navigation.openRecord("audiences", { id: "a1" }));
    expect(result.current.requests.audiences).toEqual({ id: "a1", revision: 3 });
    act(() => result.current.navigation.openRecord("automations", { id: "r1" }));
    expect(result.current.requests.automations).toEqual({ id: "r1", revision: 1 });
    expect(result.current.requests.campaigns).toBeUndefined();
    expect(navigate).toHaveBeenCalledTimes(4);
  });

  it("asks for the capture inbox, as a link from a page", () => {
    const navigate = vi.fn();
    const { result } = renderHook(() => useEmailRequests(navigate));

    act(() => result.current.navigation.openInbox());

    expect(navigate).toHaveBeenCalledWith("inbox", { fromContent: true });
    // There is no record in it to ask for.
    expect(result.current.requests).toEqual({});
  });

  it("asks for Settings scoped to an organization", () => {
    const navigate = vi.fn();
    const { result } = renderHook(() => useEmailRequests(navigate));

    act(() => result.current.navigation.openSettings("acme"));
    act(() => result.current.navigation.openSettings("acme"));

    expect(navigate).toHaveBeenCalledWith("settings", { fromContent: true });
    expect(result.current.requests.settings).toEqual({ accountId: "acme", revision: 2 });
  });

  it("keeps its openers the same across renders and uses the navigate it was last given", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { result, rerender } = renderHook(({ navigate }) => useEmailRequests(navigate), { initialProps: { navigate: first } });
    const opened = result.current.navigation;

    rerender({ navigate: second });
    expect(result.current.navigation).toBe(opened);
    act(() => result.current.navigation.openRecord("campaigns", { id: "c1" }));

    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledWith("campaigns", { fromContent: true });
  });

  it("gives a section the openers, and says so when there is no Email app to open anything in", () => {
    const openers = { openRecord: vi.fn(), openSettings: vi.fn(), openInbox: vi.fn() };
    const wrapper = ({ children }: { children: ReactNode }) => (
      <EmailNavigationContext.Provider value={openers}>{children}</EmailNavigationContext.Provider>
    );
    const { result } = renderHook(() => ({ record: useOpenRecord(), settings: useOpenSettings(), inbox: useOpenInbox() }), { wrapper });
    result.current.record("templates", { add: true });
    result.current.settings("acme");
    result.current.inbox();
    expect(openers.openRecord).toHaveBeenCalledWith("templates", { add: true });
    expect(openers.openSettings).toHaveBeenCalledWith("acme");
    expect(openers.openInbox).toHaveBeenCalledOnce();

    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      expect(() => renderHook(() => useOpenRecord())).toThrow(/outside the Email app/);
      expect(() => renderHook(() => useOpenSettings())).toThrow(/outside the Email app/);
      expect(() => renderHook(() => useOpenInbox())).toThrow(/outside the Email app/);
    } finally {
      quiet.mockRestore();
    }
  });
});

// ---------------------------------------------------------------------------
// What a section does with a request
// ---------------------------------------------------------------------------

describe("a section given an open request", () => {
  /** A section on its own, over the feeds the app root would retain for it, with
   *  the openers a page in it reaches for. */
  function Section({ name, request }: { name: OpenSection; request?: OpenRequest }) {
    const { navigation } = useEmailRequests(() => {});
    return (
      <EmailNavigationContext.Provider value={navigation}>
        <SectionBody name={name} request={request} />
      </EmailNavigationContext.Provider>
    );
  }

  function SectionBody({ name, request }: { name: OpenSection; request?: OpenRequest }) {
    const feeds = useEmailFeeds();
    const recordFeeds = useEmailRecordFeeds(feeds);
    const writes = useCampaignWrites();
    const [uploads] = useState(fakeUploads);
    switch (name) {
      case "campaigns":
        return <CampaignsSection feeds={feeds} recordFeeds={recordFeeds} trackByDefault openRequest={request} />;
      case "audiences":
        return <AudiencesSection feeds={feeds} recordFeeds={recordFeeds} writes={writes} includeArchived={false} uploads={uploads} openRequest={request} />;
      case "templates":
        return <TemplatesSection feeds={feeds} includeArchived={false} openRequest={request} />;
      case "automations":
        return <AutomationsSection feeds={feeds} writes={writes} includeArchived={false} openRequest={request} />;
    }
  }

  function mountSection(name: OpenSection, seed: FakeSeed = {}) {
    h.connection = fakeConnection(seed);
    const view = render(withSession(<Section name={name} />));
    return { view, request: (request?: OpenRequest) => view.rerender(withSession(<Section name={name} request={request} />)) };
  }

  it.each([
    ["campaigns", "Add a campaign", "Campaigns"],
    ["audiences", "Add an audience", "Audiences"],
    ["templates", "Create new template", "Templates"],
    ["automations", "Add an automation", "Automations"],
  ] as const)("starts the %s Add flow", async (name, adding, list) => {
    const { request } = mountSection(name);
    expect(await heading(list)).toBeTruthy();

    request({ add: true, revision: 1 });

    expect(await heading(adding)).toBeTruthy();
    expect(screen.getByRole("button", { name: `Back to ${list}` })).toBeTruthy();
  });

  const records: [OpenSection, FakeSeed, string, string][] = [
    ["campaigns", { campaigns: [campaignRow({ id: "x1" })] }, "August update", "Campaigns"],
    ["audiences", { audiences: [audienceRow({ id: "x1" })] }, "Newsletter", "Audiences"],
    ["templates", { templates: [templateRow({ id: "x1" })] }, "August copy", "Templates"],
    ["automations", { emailRules: [ruleRow({ id: "x1" })] }, "Tell the owner about new admins", "Automations"],
  ];

  it.each(records)("opens the %s record it names", async (name, seed, title, list) => {
    const { request } = mountSection(name, seed);
    await screen.findByRole("heading", { level: 3, name: list });

    request({ id: "x1", revision: 1 });

    expect(await heading(title)).toBeTruthy();
    expect(screen.getByRole("button", { name: `Back to ${list}` })).toBeTruthy();
  });

  it("acts on a request once per revision, so going back is not undone by a render", async () => {
    const { request } = mountSection("templates", { templates: [templateRow({ id: "x1" })] });
    await heading("Templates");
    const asked = { id: "x1", revision: 1 } as const;

    request(asked);
    await heading("August copy");
    fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
    expect(await heading("Templates")).toBeTruthy();

    // The same request again (the app re-rendered): nothing happens.
    request({ ...asked });
    expect(screen.queryByRole("heading", { level: 3, name: "August copy" })).toBeNull();

    // A new revision of it: the record opens again.
    request({ id: "x1", revision: 2 });
    expect(await heading("August copy")).toBeTruthy();
  });

  it("lets a request to open a record replace an Add flow, and an Add replace a page", async () => {
    const { request } = mountSection("audiences", { audiences: [audienceRow({ id: "x1" })] });
    await heading("Audiences");

    request({ add: true, revision: 1 });
    expect(await heading("Add an audience")).toBeTruthy();

    request({ id: "x1", revision: 2 });
    expect(await heading("Newsletter")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Add an audience" })).toBeNull();

    request({ add: true, revision: 3 });
    expect(await heading("Add an audience")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Newsletter" })).toBeNull();

    // Going back from the Add lands on the list, not on the page it replaced.
    fireEvent.click(screen.getByRole("button", { name: "Back to Audiences" }));
    expect(await heading("Audiences")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Newsletter" })).toBeNull();
  });
});
