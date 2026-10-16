import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { fakeConnection, withSession, appSessionRow } = await import("./harness");
const { MachineActivity } = await import("../../src/apps/fleet/machines/MachineActivity");
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const activity = (over = {}) => ({ id: "a", workerId: "machine", ownerUserId: "me", kind: "model", label: "local-model · chat", status: "queued", startedAt: "2026-10-16T10:00:00Z", expiresAt: "2099-01-01T00:00:00Z", ...over });
function mount(seed = {}) { const c = fakeConnection(seed); h.connection = c; render(withSession(<MachineActivity workerId="machine" />, { userId: "me" })); return c; }
it("seeds older active work, folds queue and completion events without rereads, and excludes another machine or owner", async () => {
 const c = mount({ activeMachineActivity: [activity()] });
 await screen.findByText("Queued");
 await act(async () => c.subscriptions.emit("v1:worker:activity", activity({ status: "running" })));
 expect(screen.getByText("Running")).toBeTruthy(); expect(screen.queryByText("Queued")).toBeNull();
 await act(async () => { c.subscriptions.emit("v1:worker:activity", activity({ id: "foreign", label: "Secret", ownerUserId: "else" })); c.subscriptions.emit("v1:worker:activity", activity({ id: "other", label: "Other machine", workerId: "else" })); });
 expect(screen.queryByText("Secret")).toBeNull(); expect(screen.queryByText("Other machine")).toBeNull();
 await act(async () => c.subscriptions.emit("v1:worker:activity", activity({ status: "succeeded", endedAt: "2026-10-16T10:01:00Z" })));
 expect(screen.getByText("Completed")).toBeTruthy();
 expect(c.query.machineActivity).toHaveBeenCalledTimes(1);
});
it("opens a separate detail page, keeps it current, and copies only from its icon", async () => {
 const writeText = vi.fn().mockResolvedValue(undefined); vi.stubGlobal("navigator", { clipboard: { writeText } });
 const c = mount({ machineActivity: [activity()] });
 fireEvent.click(await screen.findByRole("button", { name: /local-model/ }));
 expect(screen.queryByRole("list", { name: "Machine activity" })).toBeNull();
 const details = within(screen.getByRole("region", { name: "Details" }));
 fireEvent.click(details.getByText("a")); expect(writeText).not.toHaveBeenCalled();
 fireEvent.click(details.getByRole("button", { name: "Copy Activity id" })); expect(writeText).toHaveBeenCalledExactlyOnceWith("a");
 await act(async () => c.subscriptions.emit("v1:worker:activity", activity({ status: "failed" })));
 expect(within(screen.getByRole("region", { name: "Overview" })).getByText("Failed")).toBeTruthy();
 fireEvent.click(screen.getByRole("button", { name: "Back to Activity" })); expect(screen.getByRole("list", { name: "Machine activity" })).toBeTruthy();
});
it("combines app sessions and recorded tool calls, with live work before completed work", async () => {
 mount({ appSessionsForUser: [appSessionRow({ id: "s", ownerUserId: "me", workerId: "machine", app: "codex", status: "running" })], invocationsForWorker: [{ id: "call", ownerUserId: "me", workerId: "machine", tool: "workerHost", action: "exec", outcome: "success", startedAt: "2090-01-01T00:00:00Z" }] });
 const list = await screen.findByRole("list", { name: "Machine activity" });
 await screen.findByText("workerHost.exec");
 const rows = within(list).getAllByRole("button"); expect(rows[0]!.textContent).toContain("Codex"); expect(rows[1]!.textContent).toContain("Completed");
});
it("does not label an expired unresolved call as still running", async () => {
 mount({ machineActivity: [activity({ status: "running", expiresAt: "2000-01-01T00:00:00Z" })] });
 await screen.findByText("Interrupted"); expect(screen.queryByText("Running")).toBeNull();
});
it("keeps partial records visible when another feed fails", async () => {
 const c = fakeConnection({ appSessionsForUser: [appSessionRow({ id: "s", ownerUserId: "me", workerId: "machine", app: "codex", status: "ended" })] }); c.query.machineActivity.mockRejectedValue(new Error("Read unavailable")); h.connection = c;
 render(withSession(<MachineActivity workerId="machine" />, { userId: "me" }));
 await screen.findByText("Activity updates are interrupted."); expect(await screen.findByText("Codex")).toBeTruthy(); expect(screen.queryByText("No activity yet")).toBeNull();
});
it("preserves earlier model history without duplicating calls covered by lifecycle receipts", async () => {
 mount({ firstMachineActivity: [activity({ status: "succeeded" })], myModelCallsOnMachine: [
  { id: "old", model: "earlier-model", outcome: "ok", createdAt: "2026-10-15T10:00:00Z" },
  { id: "new", model: "duplicate-model", outcome: "ok", createdAt: "2026-10-16T10:01:00Z" },
 ] });
 expect(await screen.findByText("earlier-model")).toBeTruthy(); expect(screen.queryByText("duplicate-model")).toBeNull();
});
