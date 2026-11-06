import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption, openSelect } from "../../selectControl";
import { readTheBar } from "../../readTheBar";
import { addressDigest } from "../../../src/apps/email/audiences/addressDigest";
import { audienceRow, fakeConnection, recipientRow, rowsResult, withSession } from "../harness";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../../src/live/connection", () => ({ useOsConnection: () => h.connection, bridgePathFor: (base: string) => base + "_memql/ws", osBridgePath: "/_memql/ws" }));
vi.mock("../../../src/auth/context", () => ({ useAuthSource: () => ({ bearer: async () => "token", refresh: async () => {} }) }));
const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const uploads = () => ({ upload: vi.fn(() => ({ done: Promise.resolve({ artifactId: "csv", title: "list.csv", fileKind: "file", source: "uploaded" }), abort: () => {} })) });
function mount(conn: ReturnType<typeof fakeConnection>, provider = uploads(), archived = false) {
  h.connection = conn;
  const store = new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} });
  const settings = { ...store.load(), includeArchived: archived };
  render(withSession(<EmailApp sectionId="audiences" navigate={() => {}} askContext={() => {}} uploads={provider} store={{ load: () => settings, save: () => {} }} />));
}
async function press(name: string) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name })); }
describe("Email audiences", () => {
  it("shows subscriber counts and confirmation, refines, and hides archived audiences", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a", name: "Newsletter", confirmation: "required" }), audienceRow({ id: "b", name: "Club" }), audienceRow({ id: "c", name: "Old", status: "archived" })], subscriberCounts: { a: 1204, b: 3 } });
    mount(conn);
    await screen.findByText("1,204 subscribers");
    expect(screen.queryByRole("button", { name: /Old/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Refine audiences" }));
    chooseOption(screen.getByLabelText("Confirmation"), "Asks to confirm");
    expect(screen.queryByRole("button", { name: /Club/ })).toBeNull();
    expect(screen.getByRole("button", { name: /Newsletter/ })).toBeTruthy();
  });
  it("asks before archiving, holds a double click, keeps the page and restores", async () => {
    const row = audienceRow({ id: "a" });
    const conn = fakeConnection({ audiences: [row] });
    conn.query.archiveAudience.mockImplementation(async () => { conn.query.audiences.mockResolvedValue(rowsResult([{ ...row, status: "archived" }])); return rowsResult([]); });
    mount(conn);
    fireEvent.click(await screen.findByText("Newsletter"));
    await press("Archive");
    fireEvent.click(screen.getByRole("button", { name: "Archive" }));
    expect(conn.query.archiveAudience).not.toHaveBeenCalled();
    await press("Archive");
    await screen.findByRole("button", { name: "Restore" });
    await press("Restore");
    await waitFor(() => expect(conn.query.restoreAudience).toHaveBeenCalledWith({ audienceId: "a" }));
    expect(document.querySelector("button:disabled")).toBeNull();
  });
  it("keeps the chosen organization and sends a pending first member", async () => {
    const conn = fakeConnection({ accounts: [{ id: "acme", name: "Acme", status: "active" }, { id: "other", name: "Other", status: "active" }] });
    conn.query.createAudience.mockImplementation(async args => { conn.subscriptions.emit("v1:campaigns:audience", audienceRow({ ...args, id: String(args.audienceId) }), "NODE_CREATED"); return rowsResult([]); });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an audience" }));
    const organization = screen.queryByRole("radio", { name: "Other" });
    if (organization) fireEvent.click(organization); else { fireEvent.click(screen.getByRole("button", { name: /Organization/ })); fireEvent.click(screen.getByRole("radio", { name: "Other" })); }
    fireEvent.change(await screen.findByLabelText("Audience name"), { target: { value: "Client newsletter" } });
    await press("Next");
    fireEvent.click(screen.getByRole("radio", { name: /Ask them to confirm by email/ }));
    await press("Next"); await press("Add audience");
    fireEvent.click(await screen.findByRole("button", { name: "Add people" }));
    fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: " subscriber@example.test " } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Subscriber" } });
    await waitFor(() => expect(conn.query.recipientInAudienceByDigest).toHaveBeenCalled());
    await press("Add");
    await waitFor(() => expect(conn.query.campaignAddRecipient).toHaveBeenCalledWith(expect.objectContaining({ accountId: "other", subscriptionStatus: "pending", email: "subscriber@example.test" })));
    expect(conn.query.createAudience.mock.calls[0]?.[0]).toMatchObject({ accountId: "other", confirmation: "required" });
  });
  it("retains the CSV report until Done and uses the shell upload provider", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })] });
    conn.query.campaignImportRecipients.mockResolvedValue(rowsResult([{ added: 412, duplicates: 38, invalid: 1, total: 451, samples: [{ line: 12, text: "bad", reason: "no @" }] }]));
    const upload = uploads(); mount(conn, upload);
    fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Import a CSV file" }));
    await act(async () => fireEvent.change(screen.getByLabelText("CSV file"), { target: { files: [new File(["email\na@b.test"], "list.csv", { type: "text/csv" })] } }));
    await screen.findByText("412 added, 38 already here, 1 could not be read from 451 rows.");
    expect(upload.upload).toHaveBeenCalledOnce();
    expect(conn.query.campaignImportRecipients).toHaveBeenCalledWith({ audienceId: "a", artifactId: "csv", hasHeader: true });
    expect(screen.getByText("bad")).toBeTruthy(); await press("Done");
    expect(screen.queryByText("bad")).toBeNull();
  });
  it("updates people from another session and changes a subscription", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })], recipientsForAudience: [recipientRow({ id: "r", email: "one@acme.test" })] }); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await screen.findByText("one@acme.test");
    chooseOption(screen.getByLabelText("Subscription state for one@acme.test"), "Unsubscribed");
    await waitFor(() => expect(conn.query.campaignSetRecipientSubscription).toHaveBeenCalledWith(expect.objectContaining({ recipientId: "r", subscriptionStatus: "unsubscribed" })));
    conn.query.recipientsForAudience.mockResolvedValue(rowsResult([recipientRow({ id: "r2", email: "two@acme.test", subscriptionStatus: "unsubscribed" })]));
    await act(async () => window.dispatchEvent(new Event("focus")));
    await screen.findByText("two@acme.test");
    expect(screen.getByText(/1 of these can't be mailed/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to Audiences" }));
    expect(await screen.findByText("Newsletter")).toBeTruthy();
  });
  it("add people refuses an address already in the audience", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })], recipientsForAudience: [recipientRow({ id: "r", email: "one@acme.test", emailDigest: await addressDigest("one@acme.test") })] }); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await screen.findByText("one@acme.test");
    fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "  One@Acme.TEST " } });
    await screen.findByText("This address is already in this audience.");
    expect(conn.query.recipientInAudienceByDigest).toHaveBeenCalledWith({ audienceId: "a", emailDigest: await addressDigest("one@acme.test") }, expect.anything());
    await readTheBar(); expect(screen.queryByRole("button", { name: "Add" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show them in the list" }));
    expect(await screen.findByLabelText("Subscription state for one@acme.test")).toBeTruthy();
    expect(conn.query.campaignAddRecipient).not.toHaveBeenCalled();
  });
  it("offers Add for a new address and shows the engine's refusal verbatim", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })] });
    conn.query.campaignAddRecipient.mockRejectedValue(new Error("This address is already in this audience.")); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@acme.test" } });
    await waitFor(() => expect(conn.query.recipientInAudienceByDigest).toHaveBeenCalled());
    await readTheBar(); await waitFor(() => expect(screen.getByRole("button", { name: "Add" })).toBeTruthy()); fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await screen.findByText("This address is already in this audience.");
    expect(conn.query.campaignAddRecipient).toHaveBeenCalledWith(expect.objectContaining({ email: "new@acme.test", subscriptionStatus: "subscribed" }));
    expect(conn.query.campaignAddRecipient.mock.calls[0]?.[0]).not.toHaveProperty("alreadyConfirmed");
  });
  it("says a confirmation email goes out only when an automation sends one", async () => {
    const confirmJourney = (status: string) => ({ id: "j1", accountId: "self", name: "Welcome series", description: "", status, trigger: { kind: "joined", audienceId: "a" }, steps: [{ id: "c1", kind: "confirm", templateId: "t1", senderIdentityId: "s1", timeoutDays: 7 }], createdAt: "2026-10-11T00:00:00Z", activatedAt: "2026-10-11T01:00:00Z" });
    const sendOnly = { ...confirmJourney("active"), id: "j2", name: "Plain welcome", steps: [{ id: "s2", kind: "send", templateId: "t1", senderIdentityId: "s1" }] };
    for (const [journeys, says] of [
      [[confirmJourney("active")], "A confirmation email goes out to this address. They are subscribed once they confirm."],
      [[confirmJourney("paused")], "Nothing sends this address a confirmation email until “Welcome series” is turned on again. They are subscribed once they confirm."],
      [[sendOnly, confirmJourney("draft")], "No automation asks the people who join this audience to confirm, so nothing sends this address a confirmation email and they stay pending."],
    ] as const) {
      const conn = fakeConnection({ audiences: [audienceRow({ id: "a", confirmation: "required" })], journeys: [...journeys] }); mount(conn);
      fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
      expect(await screen.findByText(says)).toBeTruthy();
      cleanup();
    }
  });
  it("says what is missing while Add is absent", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a", confirmation: "required" })], recipientsForAudience: [recipientRow({ id: "r", email: "one@acme.test", emailDigest: await addressDigest("one@acme.test") })] }); mount(conn);
    const word = () => screen.getByRole("group", { name: "What you can do with this" }).querySelector(".os-actbar-word")?.textContent ?? "";
    fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" }));
    expect(word()).toBe("Choose how to add people");
    fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    expect(word()).toBe("Enter an email address");
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "one@acme.test" } });
    await waitFor(() => expect(word()).toBe("Already in this audience"));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@acme.test" } });
    await waitFor(() => expect(word()).toBe("Ready to add"));
    fireEvent.click(screen.getByRole("switch", { name: "Ask them to confirm" }));
    expect(word()).toBe("Say they already agreed");
    expect(screen.queryByRole("button", { name: "Add" })).toBeNull();
    fireEvent.click(screen.getByRole("switch", { name: "They already agreed to receive these emails" }));
    expect(word()).toBe("Ready to add");
    await readTheBar(); expect(screen.getByRole("button", { name: "Add" })).toBeTruthy();
  });
  it("adds someone as subscribed in a confirming audience only after the operator says they agreed", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a", confirmation: "required" })], journeys: [{ id: "j1", accountId: "self", name: "Welcome series", description: "", status: "active", trigger: { kind: "joined", audienceId: "a" }, steps: [{ id: "c1", kind: "confirm", templateId: "t1", senderIdentityId: "s1", timeoutDays: 7 }], createdAt: "2026-10-11T00:00:00Z", activatedAt: "2026-10-11T01:00:00Z" }] }); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@acme.test" } });
    expect(screen.getByText(/confirmation email goes out/)).toBeTruthy();
    fireEvent.click(screen.getByRole("switch", { name: "Ask them to confirm" }));
    await waitFor(() => expect(conn.query.recipientInAudienceByDigest).toHaveBeenCalled()); await readTheBar();
    expect(screen.queryByRole("button", { name: "Add" })).toBeNull();
    fireEvent.click(screen.getByRole("switch", { name: "They already agreed to receive these emails" })); await readTheBar();
    fireEvent.click(await screen.findByRole("button", { name: "Add" }));
    await waitFor(() => expect(conn.query.campaignAddRecipient).toHaveBeenCalledWith(expect.objectContaining({ subscriptionStatus: "subscribed", alreadyConfirmed: true })));
  });
  it("marks someone subscribed in a confirming audience only after the operator says they agreed", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a", confirmation: "required" })], recipientsForAudience: [recipientRow({ id: "r", email: "one@acme.test", subscriptionStatus: "pending" })] }); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await screen.findByText("one@acme.test");
    chooseOption(screen.getByLabelText("Subscription state for one@acme.test"), "Subscribed");
    expect(conn.query.campaignSetRecipientSubscription).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Mark subscribed" })).toBeNull();
    await readTheBar();
    fireEvent.click(screen.getByRole("switch", { name: "They already agreed to receive these emails" }));
    await screen.findByRole("button", { name: "Mark subscribed" }); await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Mark subscribed" }));
    await waitFor(() => expect(conn.query.campaignSetRecipientSubscription).toHaveBeenCalledWith(expect.objectContaining({ recipientId: "r", subscriptionStatus: "subscribed", alreadyConfirmed: true })));
  });
  it("offers only the states an operator can set", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })], recipientsForAudience: [recipientRow({ id: "r", email: "one@acme.test" })] }); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter"));
    const choice = await screen.findByLabelText("Subscription state for one@acme.test");
    const offered = within(openSelect(choice)).getAllByRole("option").map(option => option.textContent);
    // Bounced and complained are the provider's verdicts, which the engine refuses from a person.
    expect(offered).toEqual(["Subscribed", "Pending confirmation", "Unsubscribed"]);
  });
  it("offers no change to subscribed for people who left, and shows the engine's refusal for others verbatim", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })], recipientsForAudience: [recipientRow({ id: "gone", email: "gone@acme.test", subscriptionStatus: "unsubscribed" }), recipientRow({ id: "r", email: "one@acme.test", subscriptionStatus: "pending" })] });
    conn.query.campaignSetRecipientSubscription.mockRejectedValue(new Error("This address is suppressed. Only the person can subscribe again.")); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await screen.findByText("one@acme.test");
    expect(screen.queryByLabelText("Subscription state for gone@acme.test")).toBeNull();
    chooseOption(screen.getByLabelText("Subscription state for one@acme.test"), "Subscribed");
    await screen.findByText("This address is suppressed. Only the person can subscribe again.");
  });
  it("shows the person the duplicate check found even when the roster page does not hold them", async () => {
    const old = recipientRow({ id: "old", email: "old@acme.test", displayName: "Old Member", emailDigest: await addressDigest("old@acme.test") });
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })], recipientsForAudience: [old] });
    conn.query.recipientsForAudience.mockResolvedValue(rowsResult([recipientRow({ id: "new", email: "new@acme.test" })])); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await screen.findByText("new@acme.test");
    expect(screen.queryByText("old@acme.test")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "old@acme.test" } });
    await screen.findByText("This address is already in this audience.");
    fireEvent.click(screen.getByRole("button", { name: "Show them in the list" }));
    expect(await screen.findByLabelText("Subscription state for old@acme.test")).toBeTruthy();
    expect(screen.getByText("new@acme.test")).toBeTruthy();
  });
  it("offers Try again when the duplicate check fails", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })] });
    conn.query.recipientInAudienceByDigest.mockRejectedValueOnce(new Error("The lookup is unavailable.")); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); fireEvent.click(screen.getByRole("button", { name: "Add people" })); fireEvent.click(screen.getByRole("radio", { name: "Add one address" }));
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@acme.test" } });
    await screen.findByText("The lookup is unavailable.");
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(conn.query.recipientInAudienceByDigest).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText("The lookup is unavailable.")).toBeNull());
  });
  it("shows a confirmation refusal verbatim and keeps the consequence", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })] });
    conn.query.setAudienceConfirmation.mockRejectedValue(new Error("This audience has a welcome template. Remove it first.")); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await press("Ask people to confirm");
    expect(screen.getByText(/New signups wait as pending/)).toBeTruthy(); await press("Ask people to confirm");
    await screen.findByText("This audience has a welcome template. Remove it first.");
  });
});
