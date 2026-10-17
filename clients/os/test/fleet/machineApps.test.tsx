import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any, open: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/ask/AskProvider", () => ({ useOpenAskWork: () => h.open }));
const { MachineApps, appStatus } = await import("../../src/apps/fleet/machines/MachineApps");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { fakeConnection, withSession, machineRow, rowsResult } = await import("./harness");
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const now = new Date();
function machine(over = {}) { return machineFromRow(machineRow({ id: "machine", ownerUserId: "me", lastSeenAt: now.toISOString(), connectedNodeId: "agent", apps: [], ...over })); }
it("keeps both supported apps visible with all facts and no disclosures", () => {
 h.connection = fakeConnection(); render(withSession(<MachineApps machine={machine({ apps: [{ id: "codex", signedIn: true, allowed: true, subscription: "present" }] })} now={now} />, { userId: "me" }));
 expect(within(screen.getByRole("region", { name: "Claude Code" })).getByText("Not installed")).toBeTruthy();
 const codex = within(screen.getByRole("region", { name: "Codex" }));
 expect(codex.getByText("Ready")).toBeTruthy(); expect(codex.getByText("Active")).toBeTruthy(); expect(codex.getByText("Allowed and signed in")).toBeTruthy();
 expect(codex.queryByRole("button")).toBeNull();
});
it("distinguishes permission, sign-in, missing inventory, offline and removed states", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: false }] }); const app = m.apps[0]!;
 expect(appStatus(m, app, now)).toBe("Permission needed");
 expect(appStatus(m, { ...app, signedIn: false }, now)).toBe("Sign-in needed");
 expect(appStatus({ ...m, appsReported: false }, undefined, now)).toBe("Not reported");
 expect(appStatus({ ...m, connectedNodeId: "" }, app, now)).toBe("Machine offline");
 expect(appStatus({ ...m, revokedAt: now.toISOString() }, app, now)).toBe("Machine removed");
});
it("starts one targeted harness goal, opens it, and waits for reported readiness", async () => {
 const c = fakeConnection(); h.connection = c;
 let finish!: (value: any) => void; c.query.createGoal.mockReturnValue(new Promise(resolve => { finish = resolve; }));
 render(withSession(<MachineApps machine={machine()} now={now} />, { userId: "me" }));
 const claude = within(screen.getByRole("region", { name: "Claude Code" }));
 fireEvent.click(claude.getByRole("button", { name: "Set up" })); fireEvent.click(claude.getByRole("button"));
 expect(c.query.createGoal).toHaveBeenCalledTimes(1);
 expect(c.query.createGoal.mock.calls[0]![0]).toMatchObject({ input: { registrationId: "machine", appId: "claude-code" }, noPaidProviders: true });
 await act(async () => finish(rowsResult([{ runId: "setup-run" }])));
 expect(h.open).toHaveBeenCalledWith("setup-run"); expect(claude.getByText("Not installed")).toBeTruthy(); expect(claude.getByRole("button", { name: "View setup" })).toBeTruthy();
});
it("keeps failed setup retryable and blocks setup on offline or someone else's machine", async () => {
 const c = fakeConnection(); h.connection = c; c.query.createGoal.mockRejectedValue(new Error("No planner"));
 const view = render(withSession(<MachineApps machine={machine()} now={now} />, { userId: "me" }));
 fireEvent.click(within(screen.getByRole("region", { name: "Codex" })).getByRole("button", { name: "Set up" }));
 await screen.findByText("No planner");
 expect(within(screen.getByRole("region", { name: "Codex" })).getByRole("button", { name: "Set up" }).hasAttribute("disabled")).toBe(false);
 view.rerender(withSession(<MachineApps machine={machine({ ownerUserId: "other" })} now={now} />, { userId: "me" })); expect(screen.queryByRole("button", { name: "Set up" })).toBeNull();
});

it("keeps reported app facts visible without offering setup when apps are disabled in Settings", () => {
 const c = fakeConnection(); h.connection = c;
 const m = machine({ ownerFeatures: { headless: true, apps: false, computerUse: true }, apps: [{ id: "codex", signedIn: true, allowed: true, subscription: "present" }] });
 render(withSession(<MachineApps machine={m} now={now} />, { userId: "me" }));
 expect(appStatus(m, m.apps[0], now)).toBe("Disabled in Settings");
 const codex = within(screen.getByRole("region", { name: "Codex" }));
 expect(codex.getByText("Active")).toBeTruthy();
 expect(codex.queryByText("Ready")).toBeNull();
 expect(screen.queryByRole("button")).toBeNull();
 expect(c.query.createGoal).not.toHaveBeenCalled();
});
