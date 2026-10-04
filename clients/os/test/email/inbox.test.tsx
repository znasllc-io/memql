import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { EmailInbox } = await import("../../src/apps/email/EmailApp");
afterEach(cleanup);
const message = { id: "m1", createdAt: "2026-10-01T12:00:00Z", from: "news@client.test", fromName: "Client", To: "person@example.test", Subject: "Verify your email", TextBody: "Verify", HTMLBody: '<a href="https://identity.example.test/verify">Verify</a>' };
function connection(mode = "capture", messages = [message]) {
  const emailInbox = vi.fn(async () => ({ rows: () => [{ mode, messages }] }));
  h.connection = { query: { emailInbox } };
  return emailInbox;
}

describe("Email inbox", () => {
  it("opens shared captured mail, preserves the sender and isolates the preview", async () => {
    const read = connection();
    render(<EmailInbox />);
    fireEvent.click(await screen.findByRole("button", { name: /Verify your email/ }));
    expect(screen.getByText("Client <news@client.test>")).toBeTruthy();
    const preview = screen.getByTitle("Email preview");
    expect(preview.getAttribute("sandbox")).not.toContain("allow-scripts");
    expect(preview.getAttribute("sandbox")).not.toContain("allow-same-origin");
    fireEvent.click(screen.getByRole("button", { name: "Show plain text" }));
    expect(screen.getByText("Verify")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Inbox" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search email" }), { target: { value: "missing" } });
    expect(screen.getByText("No matching email")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Refresh inbox" }));
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });
  it("does not claim external inbound mail is connected", async () => {
    connection("external", []);
    render(<EmailInbox />);
    expect(await screen.findByText("Receiving is not connected")).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Search email" })).toBeNull();
  });
  it("keeps loaded mail on a failed refresh and clears it when the session disconnects", async () => {
    const read = connection();
    const view = render(<EmailInbox />);
    await screen.findByRole("button", { name: /Verify your email/ });
    read.mockRejectedValueOnce(new Error("Connection lost"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh inbox" }));
    await screen.findByText("Could not read email.");
    expect(screen.getByRole("button", { name: /Verify your email/ })).toBeTruthy();
    h.connection = null;
    view.rerender(<EmailInbox />);
    await waitFor(() => expect(screen.queryByRole("button", { name: /Verify your email/ })).toBeNull());
  });
});
