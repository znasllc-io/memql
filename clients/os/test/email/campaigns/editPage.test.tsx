import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { readTheBar } from "../../readTheBar";
import { chooseOption, openSelect } from "../../selectControl";
import { installSeededAccess } from "../../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { audienceRow, campaignRow, fakeConnection, senderRow, templateRow, withSession } = await import("../harness");

// A CAMPAIGN'S EDIT PAGE (memql#6033, Ruling 7): the fields of a draft, or of
// the draft a series repeats from, and one bar. Save writes only what changed;
// Cancel campaign asks by name first. Pinned in one zone with no daylight saving.

type Conn = ReturnType<typeof fakeConnection>;

const ZONE = "America/Phoenix";
let previousZone: string | undefined;

beforeAll(() => {
  previousZone = process.env.TZ;
  process.env.TZ = ZONE;
});

afterAll(() => {
  if (previousZone === undefined) delete process.env.TZ;
  else process.env.TZ = previousZone;
});

afterEach(() => {
  installSeededAccess("owner");
  localStorage.clear();
});

function seed(over: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    audiences: [audienceRow({ id: "a1", name: "Newsletter" }), audienceRow({ id: "a2", name: "Signups" })],
    templates: [templateRow({ id: "t1", name: "August copy" }), templateRow({ id: "t2", name: "Autumn draft", status: "draft" })],
    senderIdentities: [senderRow({ id: "s1", address: "news@acme.com" })],
    subscriberCounts: { a1: 1204, a2: 86 },
    campaigns: [campaignRow({ id: "c1", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" })],
    ...over,
  });
}

function mountWindow(connection: Conn, session: { role?: string; accountIds?: string[]; everyAccount?: boolean } = {}) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  function Window() {
    const [current, setCurrent] = useState("campaigns");
    return withSession(
      <EmailApp sectionId={current} navigate={(id) => setCurrent(id)} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />,
      session,
    );
  }
  return render(<Window />);
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barDetail = () => bar().querySelector(".os-actbar-detail")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));
/** The bar's acts that can be pressed now: not disabled. */
const pressable = () => within(bar()).queryAllByRole("button").filter((button) => !(button as HTMLButtonElement).disabled).map((button) => button.textContent);

const CAMPAIGN = "v1:campaigns:campaign";
const SERIES = "v1:campaigns:campaignSeries";
const optionNames = (label: string) => within(openSelect(screen.getByLabelText(label))).getAllByRole("option").map((option) => option.textContent);

async function openEdit(name = "August update") {
  fireEvent.click(await screen.findByText(name));
  expect(await screen.findByRole("heading", { level: 3, name })).toBeTruthy();
  await waitFor(() => expect(barActs()).toContain("Edit"));
  // The page's acts arrive as its reads land, and a bar that has just changed takes no press for a moment.
  await readTheBar();
  press("Edit");
  expect(await screen.findByRole("heading", { level: 3, name: "Edit campaign" })).toBeTruthy();
  await readTheBar();
}

/** The arguments the one write sent, and nothing it left out. */
function updated(conn: Conn): Record<string, unknown> {
  return conn.query.updateCampaign.mock.calls[0]![0];
}

describe("editing a campaign", () => {
  it("saves only what changed", async () => {
    const conn = seed();
    mountWindow(conn);
    await openEdit();

    // Nothing changed yet: nothing to save, and nothing to discard.
    expect(barWord()).toBe("Editing");
    expect(barActs()).toEqual(["Cancel campaign"]);

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Spring sale" } });
    fireEvent.change(screen.getByLabelText("Reply to"), { target: { value: "help@acme.com" } });
    expect(barActs()).toEqual(["Cancel campaign", "Discard changes", "Save"]);
    // Save joined the bar with the first change, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Save");

    await waitFor(() => expect(conn.query.updateCampaign).toHaveBeenCalledOnce());
    // The three the engine requires, and the two that changed -- no others.
    expect(updated(conn)).toEqual({ campaignId: "c1", name: "Spring sale", audienceId: "a1", templateId: "t1", replyTo: "help@acme.com" });
    expect(Object.keys(updated(conn)).sort()).toEqual(["audienceId", "campaignId", "name", "replyTo", "templateId"]);
    expect(conn.query.campaignConfigureSeries).not.toHaveBeenCalled();
    // Back on the campaign's page.
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
  });

  it("clears what is emptied, and saves a time for the draft to be scheduled at", async () => {
    const conn = seed({ campaigns: [campaignRow({ id: "c1", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", replyTo: "help@acme.com" })] });
    mountWindow(conn);
    await openEdit();
    fireEvent.change(screen.getByLabelText("Reply to"), { target: { value: "" } });
    chooseOption(screen.getByLabelText("When"), "Schedule");
    expect(barDetail()).toBe("Choose a time");
    fireEvent.change(screen.getByLabelText("Send at · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
    await readTheBar();
    press("Save");
    await waitFor(() => expect(conn.query.updateCampaign).toHaveBeenCalledOnce());
    expect(updated(conn)).toEqual({ campaignId: "c1", name: "August update", audienceId: "a1", templateId: "t1", replyTo: "", scheduledAt: "2031-10-14T16:00:00Z" });
  });

  it("discards changes without writing anything", async () => {
    const conn = seed();
    mountWindow(conn);
    await openEdit();
    chooseOption(screen.getByLabelText("Audience"), "Signups");
    await readTheBar();
    press("Discard changes");
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
    expect(conn.query.updateCampaign).not.toHaveBeenCalled();
  });

  it("offers published templates first and names a draft one", async () => {
    mountWindow(seed());
    await openEdit();
    expect(optionNames("Template")).toEqual(["August copy", "Autumn draft (Draft)"]);
  });

  it("Cancel campaign asks by name and cancels the draft", async () => {
    const conn = seed();
    mountWindow(conn);
    await openEdit();

    press("Cancel campaign");
    expect(within(bar()).getByText("Cancel “August update”? It won't be sent, and it stays on the list as cancelled.")).toBeTruthy();
    expect(barActs()).toEqual(["Keep", "Cancel campaign"]);
    // The second click of a double-click is not an answer.
    press("Cancel campaign");
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();
    await readTheBar();
    press("Keep");
    expect(barActs()).toEqual(["Cancel campaign"]);

    // Keep put the acts back, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Cancel campaign");
    await readTheBar();
    press("Cancel campaign");
    await waitFor(() => expect(conn.query.cancelCampaign).toHaveBeenCalledWith({ campaignId: "c1" }));
    // Taken, not yet arrived: the act stays busy and nothing else can be pressed.
    await waitFor(() => expect(barWord()).toBe("Cancelling"));
    expect(barActs()).toEqual(["Cancel campaign"]);
    expect(pressable()).toEqual([]);
    act(() => conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: "c1", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", status: "cancelled" })));
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
    await waitFor(() => expect(barWord()).toBe("Cancelled"));
  });

  it("a save whose answer never came back says so until the campaign is read back", async () => {
    const conn = seed();
    conn.query.updateCampaign.mockRejectedValueOnce(Object.assign(new Error("stream closed"), { reason: "transport" }));
    mountWindow(conn);
    await openEdit();
    const reads = conn.query.campaigns.mock.calls.length;
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "September update" } });
    await readTheBar();
    press("Save");
    expect(await screen.findByText("Couldn't confirm that your changes were saved.")).toBeTruthy();
    expect(screen.queryByText("Your changes weren't saved.")).toBeNull();
    await waitFor(() => expect(conn.query.campaigns.mock.calls.length).toBeGreaterThan(reads));
    // It went: the campaign comes back as it was saved, and what could not be confirmed no longer needs saying.
    act(() =>
      conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: "c1", name: "September update", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", createdAt: "2026-08-02T00:00:00Z" })),
    );
    await waitFor(() => expect(screen.queryByText("Couldn't confirm that your changes were saved.")).toBeNull());
    // And nothing is left to save.
    expect(barActs()).toEqual(["Cancel campaign"]);
  });

  it("a cancel whose answer never came back says so, and reads the campaign again", async () => {
    const conn = seed();
    conn.query.cancelCampaign.mockRejectedValueOnce(Object.assign(new Error("stream closed"), { reason: "transport" }));
    mountWindow(conn);
    await openEdit();
    const reads = conn.query.campaigns.mock.calls.length;
    press("Cancel campaign");
    await readTheBar();
    press("Cancel campaign");
    expect(await screen.findByText("Couldn't confirm that it was cancelled.")).toBeTruthy();
    expect(screen.queryByText("The campaign wasn't cancelled.")).toBeNull();
    await waitFor(() => expect(conn.query.campaigns.mock.calls.length).toBeGreaterThan(reads));
  });

  it("a double-click on Cancel campaign beside unsaved changes does not cancel", async () => {
    // [Cancel campaign, Discard changes, Save] becomes [Keep, Cancel campaign]:
    // the confirmation is at another place than the ask, and still under the
    // pointer's reach. The question takes no press until it can be read.
    const conn = seed();
    mountWindow(conn);
    await openEdit();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "September update" } });
    await waitFor(() => expect(barActs()).toEqual(["Cancel campaign", "Discard changes", "Save"]));
    await readTheBar();
    press("Cancel campaign");
    press("Cancel campaign");
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();
    expect(barActs()).toEqual(["Keep", "Cancel campaign"]);
  });

  it("a double-click on Keep beside unsaved changes does not discard them", async () => {
    // Keep puts [Cancel campaign, Discard changes, Save] back, and Discard
    // changes now stands where Keep stood: the rest of the double-click on
    // Keep must not throw the edits away.
    const conn = seed();
    mountWindow(conn);
    await openEdit();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "September update" } });
    await waitFor(() => expect(barActs()).toEqual(["Cancel campaign", "Discard changes", "Save"]));
    await readTheBar();
    press("Cancel campaign");
    await readTheBar();
    press("Keep");
    press("Discard changes");
    expect(barActs()).toEqual(["Cancel campaign", "Discard changes", "Save"]);
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("September update");
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();
  });

  it("defaults to the client's organization, filters resources and sends no owner", async () => {
    installSeededAccess("writer");
    const conn = seed({
      accounts: [{ id: "acme", name: "Acme", status: "active" }],
      audiences: [audienceRow({ id: "a-acme", accountId: "acme", name: "Acme audience" }), audienceRow({ id: "a-other", accountId: "other", name: "Other audience" })],
      templates: [templateRow({ id: "t-acme", accountId: "acme", name: "Acme copy" }), templateRow({ id: "t-other", accountId: "other", name: "Other copy" })],
      senderIdentities: [senderRow({ id: "s-acme", accountId: "acme", address: "news@acme.test" })],
      // A campaign from before organizations, which belongs to nobody yet.
      campaigns: [campaignRow({ id: "c1", name: "Acme newsletter", accountId: "", audienceId: "a-acme", templateId: "t-acme", senderIdentityId: "" })],
    });
    mountWindow(conn, { role: "writer", everyAccount: false, accountIds: ["acme"] });
    await openEdit("Acme newsletter");

    // One organization to be in: no question about it.
    expect(screen.queryByLabelText("Organization")).toBeNull();
    expect(optionNames("Audience")).toEqual(["Acme audience"]);
    expect(optionNames("Template")).toEqual(["Acme copy"]);
    // A client organization sends as one of its own senders.
    expect(barDetail()).toBe("Choose a sender");
    expect(barActs()).not.toContain("Save");
    chooseOption(screen.getByLabelText("Sender"), "news@acme.test");
    await readTheBar();
    press("Save");

    await waitFor(() => expect(conn.query.updateCampaign).toHaveBeenCalledOnce());
    expect(updated(conn)).toMatchObject({ campaignId: "c1", accountId: "acme", audienceId: "a-acme", templateId: "t-acme", senderIdentityId: "s-acme" });
    expect(updated(conn)).not.toHaveProperty("ownerUserId");
  });

  it("a deliberate organization change clears the audience, template and sender", async () => {
    installSeededAccess("writer");
    const conn = seed({
      accounts: [{ id: "other", name: "Other", status: "active" }, { id: "acme", name: "Acme", status: "active" }],
      audiences: [audienceRow({ id: "a-acme", accountId: "acme", name: "Acme audience" }), audienceRow({ id: "a-other", accountId: "other", name: "Other audience" })],
      templates: [templateRow({ id: "t-acme", accountId: "acme", name: "Acme copy" })],
      senderIdentities: [senderRow({ id: "s-acme", accountId: "acme", address: "news@acme.test" })],
      campaigns: [campaignRow({ id: "c1", name: "Acme newsletter", accountId: "acme", audienceId: "a-acme", templateId: "t-acme", senderIdentityId: "s-acme" })],
    });
    mountWindow(conn, { role: "writer", everyAccount: false, accountIds: ["other", "acme"] });
    await openEdit("Acme newsletter");

    expect(screen.getByLabelText("Organization").textContent).toContain("Acme");
    chooseOption(screen.getByLabelText("Organization"), "Other");
    expect(screen.getByLabelText("Audience").textContent).toContain("Choose an audience");
    expect(screen.getByLabelText("Template").textContent).toContain("Choose a template");
    expect(screen.getByLabelText("Sender").textContent).toContain("Choose a sender");
    expect(barDetail()).toBe("Choose an audience");
    expect(barActs()).toEqual(["Cancel campaign", "Discard changes"]);
    expect(conn.query.updateCampaign).not.toHaveBeenCalled();
  });

  it("does not offer to move a repeating campaign to another organization", async () => {
    // A series never changes organizations -- the engine refuses it -- so a
    // move would save the campaign under one and leave its series under the
    // other. The campaign a series repeats from is edited where it is.
    installSeededAccess("writer");
    const conn = seed({
      accounts: [{ id: "other", name: "Other", status: "active" }, { id: "acme", name: "Acme", status: "active" }],
      audiences: [audienceRow({ id: "a-acme", accountId: "acme", name: "Acme audience" })],
      templates: [templateRow({ id: "t-acme", accountId: "acme", name: "Acme copy" })],
      senderIdentities: [senderRow({ id: "s-acme", accountId: "acme", address: "news@acme.test" })],
      campaigns: [campaignRow({ id: "c1", name: "Acme newsletter", accountId: "acme", audienceId: "a-acme", templateId: "t-acme", senderIdentityId: "s-acme" })],
      campaignSeries: [
        { id: "series", sourceCampaignId: "c1", accountId: "acme", name: "Acme newsletter", status: "active", intervalWeeks: 2, timeZone: ZONE, anchorAt: "2031-10-14T16:00:00Z", nextAt: "2031-10-14T16:00:00Z", createdAt: "2027-10-01T12:00:00Z", lastError: "" },
      ],
    });
    mountWindow(conn, { role: "writer", everyAccount: false, accountIds: ["other", "acme"] });
    await openEdit("Acme newsletter");
    expect(screen.getByLabelText("Every")).toBeTruthy();
    expect(screen.queryByLabelText("Organization")).toBeNull();
  });

  it("choosing Repeat turns Save into Start repeating", async () => {
    const conn = seed();
    mountWindow(conn);
    await openEdit();

    chooseOption(screen.getByLabelText("When"), "Repeat");
    expect(barDetail()).toBe("Choose a time");
    expect(barActs()).toEqual(["Cancel campaign", "Discard changes"]);
    chooseOption(screen.getByLabelText("Every"), "2 weeks");
    fireEvent.change(screen.getByLabelText("Starting · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
    expect(barActs()).toEqual(["Cancel campaign", "Discard changes", "Start repeating"]);

    // Starting it is a send to an audience on its own clock: it asks, naming them.
    await readTheBar();
    press("Start repeating");
    expect(within(bar()).getByText("Send “August update” to Newsletter every 2 weeks from Tue, Oct 14 at 9:00?")).toBeTruthy();
    expect(barActs()).toEqual(["Not yet", "Start repeating"]);
    await readTheBar();
    press("Start repeating");
    await waitFor(() =>
      expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({
        campaignId: "c1",
        action: "save",
        intervalWeeks: 2,
        firstSendAt: "2031-10-14T16:00:00Z",
        timeZone: "America/Phoenix",
      }),
    );
    expect(conn.query.campaignConfigureSeries.mock.calls[0]![0]).not.toHaveProperty("expectedRevision");
    // Nothing on the campaign itself changed.
    expect(conn.query.updateCampaign).not.toHaveBeenCalled();
    // Taken, not yet arrived: the page stays, holding the act, until the series does.
    await waitFor(() => expect(barWord()).toBe("Starting"));
    expect(barActs()).toEqual(["Start repeating"]);
    expect(pressable()).toEqual([]);
    act(() =>
      conn.subscriptions.emit(SERIES, {
        id: "series",
        sourceCampaignId: "c1",
        accountId: "self",
        name: "August update",
        status: "active",
        intervalWeeks: 2,
        timeZone: ZONE,
        anchorAt: "2031-10-14T16:00:00Z",
        nextAt: "2031-10-14T16:00:00Z",
        createdAt: "2027-10-01T12:00:00Z",
        lastError: "",
      }),
    );
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
    await waitFor(() => expect(barWord()).toBe("Repeating"));
  });

  it("says in the engine's words when a save is refused, and keeps what was typed", async () => {
    const conn = seed();
    conn.query.updateCampaign.mockRejectedValueOnce(new Error("updateCampaign: the template belongs to another organization"));
    mountWindow(conn);
    await openEdit();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Spring sale" } });
    await readTheBar();
    press("Save");
    expect(await screen.findByText("Your changes weren't saved.")).toBeTruthy();
    expect(screen.getByText(/the template belongs to another organization/)).toBeTruthy();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Spring sale");
    expect(screen.getByRole("heading", { level: 3, name: "Edit campaign" })).toBeTruthy();
  });
});
