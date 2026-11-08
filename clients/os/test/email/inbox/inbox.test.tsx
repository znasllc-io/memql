import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../../src/apps/accounts/tie", () => ({ useAccountOptions: () => [{ id: "acme", name: "Acme" }, { id: "other", name: "Other" }] }));
const { disposeSessionCollections } = await import("../../../src/live/sessionCollections");
const { InboxSection: EmailInbox } = await import("../../../src/apps/email/inbox/InboxSection");
afterEach(() => { cleanup(); if (h.connection) disposeSessionCollections(h.connection as never); });
const message = { id: "m1", createdAt: "2026-10-01T12:00:00Z", from: "news@client.test", fromName: "Client", To: "person@example.test", Subject: "Verify your email", TextBody: "Verify", accountId: "acme", HTMLBody: '<a style="color:#2b2b2b" href="https://identity.example.test/verify">Verify</a>' };
function connection(mode = "capture", messages = [message]) {
  const emailInbox = vi.fn(async (args: { search?: string; accountId?: string; cursor?: string } = {}) => {
    const matches = messages.filter(row => (!args.accountId || row.accountId === args.accountId) && (!args.search || `${row.Subject} ${row.To} ${row.from} ${row.fromName}`.toLowerCase().includes(args.search.toLowerCase())));
    const offset = Number(args.cursor ?? 0);
    return { rows: () => [{ mode, messages: matches.slice(offset, offset + 10).map(({ TextBody: _, HTMLBody: __, ...row }) => row), nextCursor: offset + 10 < matches.length ? String(offset + 10) : "" }] };
  });
  const emailInboxMessage = vi.fn(async (args: { messageId: string }) => ({ rows: () => [{ message: messages.find(row => row.id === args.messageId) ?? null }] }));
  h.connection = { status: "connected", query: { emailInboxPage: emailInbox, emailInboxMessage } };
  return emailInbox;
}

describe("Email inbox", () => {
  it("opens shared captured mail, preserves the sender and isolates the preview", async () => {
    const read = connection();
    render(<EmailInbox />);
    fireEvent.click(await screen.findByRole("button", { name: /Verify your email/ }));
    expect(await screen.findByText("Client <news@client.test>")).toBeTruthy();
    const preview = screen.getByTitle("Email preview");
    expect(preview.getAttribute("srcdoc")).toContain("color:#2b2b2b");
    expect(preview.getAttribute("sandbox")).not.toContain("allow-scripts");
    expect(preview.getAttribute("sandbox")).not.toContain("allow-same-origin");
    fireEvent.click(screen.getByRole("button", { name: "Plain text" }));
    expect(screen.getByText("Verify")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Inbox" }));
    fireEvent.click(screen.getByRole("button", { name: "Refine inbox" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "missing" } });
    expect(await screen.findByText("No matching email")).toBeTruthy();
    fireEvent.focus(window);
    await waitFor(() => expect(read.mock.calls.length).toBeGreaterThanOrEqual(2));
  });
  it("retains loaded pages and fetches bodies only for the selected message", async () => {
    const messages = Array.from({ length: 25 }, (_, i) => ({ ...message, id: `m${i}`, Subject: `Captured ${i}` }));
    const read = connection("capture", messages);
    const original = h.connection as { query: { emailInboxMessage: ReturnType<typeof vi.fn> } };
    const first = render(<EmailInbox />);
    await screen.findByRole("button", { name: /Captured 0/ });
    expect(screen.queryByRole("button", { name: /Captured 10/ })).toBeNull();
    expect(original.query.emailInboxMessage).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByRole("button", { name: /Captured 19/ });
    first.unmount();
    render(<EmailInbox />);
    await screen.findByRole("button", { name: /Captured 19/ });
    expect(read).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: /Captured 19/ }));
    await screen.findByText("Client <news@client.test>");
    expect(original.query.emailInboxMessage).toHaveBeenCalledExactlyOnceWith({ messageId: "m19" }, expect.anything());
  });
  it("does not claim a sparse search is exhausted while older messages remain", async () => {
    const read = connection("capture", []);
    read.mockResolvedValueOnce({ rows: () => [{ mode: "capture", messages: [], nextCursor: "scan-next" }] });
    read.mockResolvedValueOnce({ rows: () => [{ mode: "capture", messages: [message], nextCursor: "" }] });
    render(<EmailInbox />);
    await screen.findByText("No matches in these messages");
    expect(screen.queryByText("No matching email")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByRole("button", { name: /Verify your email/ });
    expect(read.mock.calls[1]?.[0]).toMatchObject({ cursor: "scan-next" });
  });
  it("does not claim external mail is captured", async () => {
    connection("external", []);
    render(<EmailInbox />);
    expect(await screen.findByText("Nothing is captured on this cluster")).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Search email" })).toBeNull();
  });
  it("filters by organization", async () => {
    connection("capture", [message, { ...message, id: "m2", accountId: "other", Subject: "Other email" }]);
    render(<EmailInbox />);
    await screen.findByRole("button", { name: /Other email/ });
    fireEvent.click(screen.getByRole("button", { name: "Refine inbox" }));
    fireEvent.click(screen.getByRole("combobox", { name: "Organization" }));
    fireEvent.click(screen.getByRole("option", { name: "Acme" }));
    expect(screen.queryByRole("button", { name: /Other email/ })).toBeNull();
    expect(await screen.findByRole("button", { name: /Verify your email/ })).toBeTruthy();
  });
  it("keeps loaded mail on a failed refresh and clears it when the session disconnects", async () => {
    const read = connection();
    const view = render(<EmailInbox />);
    await screen.findByRole("button", { name: /Verify your email/ });
    read.mockRejectedValueOnce(new Error("Connection lost"));
    fireEvent.focus(window);
    await screen.findByText("Could not read email.");
    expect(await screen.findByRole("button", { name: /Verify your email/ })).toBeTruthy();
    h.connection = null;
    view.rerender(<EmailInbox />);
    await waitFor(() => expect(screen.queryByRole("button", { name: /Verify your email/ })).toBeNull());
  });
});
