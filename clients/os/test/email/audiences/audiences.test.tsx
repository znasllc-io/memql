import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption } from "../../selectControl";
import { readTheBar } from "../../readTheBar";
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
async function press(name: string) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name, exact: true })); }
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
    fireEvent.click(screen.getByRole("button", { name: "Archive", exact: true }));
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
    await press("Add");
    await waitFor(() => expect(conn.query.addRecipient).toHaveBeenCalledWith(expect.objectContaining({ accountId: "other", subscriptionStatus: "pending", email: "subscriber@example.test" })));
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
    await waitFor(() => expect(conn.query.setRecipientSubscription).toHaveBeenCalledWith(expect.objectContaining({ recipientId: "r", subscriptionStatus: "unsubscribed" })));
    conn.query.recipientsForAudience.mockResolvedValue(rowsResult([recipientRow({ id: "r2", email: "two@acme.test", subscriptionStatus: "unsubscribed" })]));
    await act(async () => window.dispatchEvent(new Event("focus")));
    await screen.findByText("two@acme.test");
    expect(screen.getByText(/1 of these can't be mailed/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to Audiences" }));
    expect(await screen.findByText("Newsletter")).toBeTruthy();
  });
  it("shows a confirmation refusal verbatim and keeps the consequence", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a" })] });
    conn.query.setAudienceConfirmation.mockRejectedValue(new Error("This audience has a welcome template. Remove it first.")); mount(conn);
    fireEvent.click(await screen.findByText("Newsletter")); await press("Ask people to confirm");
    expect(screen.getByText(/New signups wait as pending/)).toBeTruthy(); await press("Ask people to confirm");
    await screen.findByText("This audience has a welcome template. Remove it first.");
  });
});
