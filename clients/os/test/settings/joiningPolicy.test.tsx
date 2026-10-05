import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ row: {} as Record<string, unknown>, save: vi.fn(), connection: {} }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/apps/settings/adminWire", () => ({ readClusterSettings: async () => h.row }));
vi.mock("../../src/apps/settings/settingsWrites", () => ({ useSettingsWrites: () => ({ busyKey: "", refusal: null, done: "", clear: vi.fn(), saveClusterSettings: h.save }) }));
import { PolicyPanel } from "../../src/apps/settings/PolicyPanel";

afterEach(cleanup);
beforeEach(() => {
  h.row = { registrationMode: "invite_only", internalDefaultRole: "writer", internalDomains: "team.test" };
  h.save.mockReset().mockImplementation(async next => { h.row = next; return true; });
});
function choose(mode: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "How people join" }));
  fireEvent.click(screen.getByRole("option", { name: mode }));
}
it.each([["Open registration", "open"], ["Approved email domains", "domain_restricted"], ["Admin approval", "waitlist"]])(
  "saves %s after setup, with only the relevant fields and pending entries included", async (label, mode) => {
    render(<PolicyPanel enabled />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove team.test" })).toBeTruthy());
    expect(screen.queryByRole("textbox", { name: "Approved email domains" })).toBeNull();
    expect(screen.queryByRole("textbox", { name: "Notify about requests (optional)" })).toBeNull();
    choose(label!);
    if (mode === "domain_restricted") {
      expect(screen.getByRole("button", { name: "Save" })).toHaveProperty("disabled", true);
      fireEvent.change(screen.getByRole("textbox", { name: "Approved email domains" }), { target: { value: " Approved.test " } });
    }
    if (mode !== "open") fireEvent.change(screen.getByRole("textbox", { name: "Notify about requests (optional)" }), { target: { value: "ops@team.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(h.save).toHaveBeenCalledOnce());
    expect(h.save.mock.calls[0]?.[0]).toMatchObject({ registrationMode: mode, internalDomains: "team.test", internalDefaultRole: "writer",
      registrationDomains: mode === "domain_restricted" ? "approved.test" : "", accessRequestNotifyEmails: mode === "open" ? "" : "ops@team.test" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toHaveProperty("disabled", true));
    choose("Invitation only");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(h.save).toHaveBeenCalledTimes(2));
    expect(h.save.mock.calls[1]?.[0]).toMatchObject({ registrationMode: "invite_only", internalDomains: "team.test" });
  });
it("preserves mode-specific values when switching and discards pending drafts explicitly", async () => {
  render(<PolicyPanel enabled />);
  await screen.findByRole("button", { name: "Remove team.test" });
  choose("Approved email domains");
  fireEvent.change(screen.getByRole("textbox", { name: "Approved email domains" }), { target: { value: "allowed.test" } });
  fireEvent.click(screen.getByRole("button", { name: "Add approved domain" }));
  choose("Invitation only"); choose("Approved email domains");
  expect(screen.getByRole("button", { name: "Remove allowed.test" })).toBeTruthy();
  fireEvent.change(screen.getByRole("textbox", { name: "Internal email domains (optional)" }), { target: { value: "https://bad.test" } });
  fireEvent.click(screen.getByRole("button", { name: "Add internal domain" }));
  expect(screen.getByRole("alert").textContent).toContain("example.com");
  expect(screen.getByRole("button", { name: "Save" })).toHaveProperty("disabled", true);
  fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
  expect(screen.getByRole("textbox", { name: "Internal email domains (optional)" })).toHaveProperty("value", "");
  expect(screen.queryByRole("textbox", { name: "Approved email domains" })).toBeNull();
  expect(h.save).not.toHaveBeenCalled();
});
