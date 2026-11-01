import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any, open: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/ask/AskProvider", () => ({ useOpenAskWork: () => h.open }));
const { MachineApps } = await import("../../src/apps/fleet/machines/MachineApps");
const { appStatus } = await import("../../src/apps/fleet/machines/features");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { builtinReply, fakeConnection, withSession, machineRow, rowsResult } = await import("./harness");
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const now = new Date();
function machine(over = {}) { return machineFromRow(machineRow({ id: "machine", ownerUserId: "me", lastSeenAt: now.toISOString(), connectedNodeId: "agent", apps: [], ...over })); }
function signInRow(over = {}) { return { id: "v1:worker:appSignIn:s1", ownerUserId: "me", workerId: "machine", app: "codex", status: "waiting", url: "https://auth.openai.com/codex/device", userCode: "ABCD-EFGH", needsCode: false, errorMessage: "", requestedAt: now.toISOString(), expiresAt: new Date(now.getTime() + 600000).toISOString(), endedAt: "", ...over }; }
const signedOut = { id: "codex", signedIn: false, allowed: true, subscription: "present", readiness: "signed_out" };
const canSignIn = [{ id: "codex", harness: "codex-app-server", signIn: true }];
/** The section's acts, leaving out the facts' copy icons. */
function acts(scope: { queryAllByRole: (role: "button") => HTMLElement[] }) { return scope.queryAllByRole("button").filter(button => !/^Copy /.test(button.getAttribute("aria-label") ?? "")); }
it("keeps both supported apps visible and asks nothing of one that is ready", () => {
 h.connection = fakeConnection(); render(withSession(<MachineApps machine={machine({ apps: [{ id: "codex", signedIn: true, allowed: true, subscription: "present", readiness: "ready", readinessCheckedAt: now.toISOString() }] })} now={now} />, { userId: "me" }));
 expect(within(screen.getByRole("region", { name: "Claude Code" })).getByText("Not installed")).toBeTruthy();
 const codex = within(screen.getByRole("region", { name: "Codex" }));
 expect(codex.getByText("Ready")).toBeTruthy(); expect(codex.getByText("Active")).toBeTruthy(); expect(codex.getByText("Last checked")).toBeTruthy();
 expect(acts(codex)).toEqual([]);
});
it("never offers a retry: availability is checked on a schedule", () => {
 h.connection = fakeConnection(); render(withSession(<MachineApps machine={machine({ apps: [{ id: "codex", signedIn: true, allowed: true, readiness: "out_of_usage", usageResetsAt: "2026-11-04T06:52:03Z" }] })} now={now} />, { userId: "me" }));
 expect(screen.queryByRole("button", { name: /retry/i })).toBeNull();
});
it("shows out of usage with when it resets, and offers nothing to do about it", () => {
 h.connection = fakeConnection(); render(withSession(<MachineApps machine={machine({ apps: [{ id: "codex", signedIn: true, allowed: true, subscription: "present", readiness: "out_of_usage", usageResetsAt: "2026-11-04T06:52:03Z", readinessCheckedAt: now.toISOString() }] })} now={now} />, { userId: "me" }));
 const codex = within(screen.getByRole("region", { name: "Codex" }));
 expect(codex.getByText("Out of usage")).toBeTruthy();
 expect(codex.getByText("Resets")).toBeTruthy();
 expect(acts(codex)).toEqual([]);
});
it("reads a machine that has measured nothing as not checked, not as ready or broken", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: true }] });
 expect(appStatus(m, m.apps[0], now)).toBe("Not checked");
 expect(m.apps[0]!.runnable).toBe(true);
});
it("reads a word this build does not know as the engine does: measured, and not ready", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: true, readiness: "a-new-word" }] });
 expect(m.apps[0]!.readiness).toBe("unknown");
 expect(m.apps[0]!.runnable).toBe(false);
});
it("offers Sign in only where this machine's Cockpit can run one, and falls back to Ask where it cannot", () => {
 h.connection = fakeConnection();
 const view = render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 const codex = () => within(screen.getByRole("region", { name: "Codex" }));
 expect(codex().getByText("Sign-in needed")).toBeTruthy();
 expect(codex().getByRole("button", { name: "Sign in to Codex" })).toBeTruthy();
 expect(codex().queryByRole("button", { name: "Connect" })).toBeNull();
 view.rerender(withSession(<MachineApps machine={machine({ apps: [{ ...signedOut, readiness: undefined }] })} now={now} />, { userId: "me" }));
 expect(codex().queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
 expect(codex().getByRole("button", { name: "Connect" })).toBeTruthy();
});
it("offers no sign-in on someone else's machine, offline, or before the app is allowed", () => {
 h.connection = fakeConnection();
 const view = render(withSession(<MachineApps machine={machine({ ownerUserId: "other", apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(screen.queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
 view.rerender(withSession(<MachineApps machine={machine({ connectedNodeId: "", lastSeenAt: "", apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(screen.queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
 view.rerender(withSession(<MachineApps machine={machine({ apps: [{ ...signedOut, allowed: false }], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(screen.queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
 expect(screen.getAllByText("Permission needed").length).toBeGreaterThan(0);
});
it("starts one sign-in for the app and machine pressed", async () => {
 const c = fakeConnection({}, { fleetAppSignIn: async () => builtinReply("fleetAppSignIn", [{ signInId: "s1", status: "requested" }]) }); h.connection = c;
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 const button = await screen.findByRole("button", { name: "Sign in to Codex" });
 await act(async () => { fireEvent.click(button); });
 expect(c.query.fleetAppSignIn).toHaveBeenCalledTimes(1);
 expect(c.query.fleetAppSignIn).toHaveBeenCalledWith({ registrationId: "machine", app: "codex" });
});
it("shows the refusal when a sign-in cannot start, and lets the person try again", async () => {
 const c = fakeConnection({}, { fleetAppSignIn: async () => { throw new Error("Laptop is not connected to the cluster right now."); } }); h.connection = c;
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 await act(async () => { fireEvent.click(await screen.findByRole("button", { name: "Sign in to Codex" })); });
 expect(await screen.findByText(/not connected to the cluster/)).toBeTruthy();
 expect(screen.getByRole("button", { name: "Sign in to Codex" }).hasAttribute("disabled")).toBe(false);
});
it("gives a device login its page and its code, and takes no pasted code", async () => {
 h.connection = fakeConnection({ appSignInsForWorker: [signInRow()] });
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 const link = await screen.findByRole("link", { name: "Open sign-in page" });
 expect(link.getAttribute("href")).toBe("https://auth.openai.com/codex/device");
 expect(link.getAttribute("rel")).toContain("noopener");
 expect(screen.getByRole("textbox", { name: "the Codex sign-in code" }).getAttribute("value")).toBe("ABCD-EFGH");
 expect(screen.queryByRole("textbox", { name: "Code from the sign-in page" })).toBeNull();
 expect(screen.queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
});
it("passes on the code a page showed, once, and clears it from the screen", async () => {
 const c = fakeConnection({ appSignInsForWorker: [signInRow({ url: "https://claude.com/cai/oauth/authorize?code=true", userCode: "", needsCode: true })] }, { fleetAppSignInCode: async () => builtinReply("fleetAppSignInCode", [{ signInId: "s1", status: "waiting" }]) }); h.connection = c;
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 const box = await screen.findByRole("textbox", { name: "Code from the sign-in page" });
 const finish = screen.getByRole("button", { name: "Finish sign-in" });
 expect(finish.hasAttribute("disabled")).toBe(true);
 fireEvent.change(box, { target: { value: "pasted#code" } });
 await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Finish sign-in" })); });
 expect(c.query.fleetAppSignInCode).toHaveBeenCalledTimes(1);
 expect(c.query.fleetAppSignInCode).toHaveBeenCalledWith({ signInId: "v1:worker:appSignIn:s1", code: "pasted#code" });
 expect(screen.queryByRole("textbox", { name: "Code from the sign-in page" })).toBeNull();
 expect(screen.getByText("Finishing the sign-in on this machine.")).toBeTruthy();
});
it("refuses to link anywhere but an https page, whatever a machine reports", async () => {
 h.connection = fakeConnection({ appSignInsForWorker: [signInRow({ url: "javascript:alert(1)" })] });
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 await screen.findByText("Starting the sign-in on this machine.");
 expect(screen.queryByRole("link", { name: "Open sign-in page" })).toBeNull();
});
it("reads a sign-in past its expiry as over, so a dead replica leaves no spinner", async () => {
 h.connection = fakeConnection({ appSignInsForWorker: [signInRow({ status: "waiting", expiresAt: new Date(now.getTime() - 1000).toISOString() })] });
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(await screen.findByRole("button", { name: "Sign in to Codex" })).toBeTruthy();
 expect(screen.queryByRole("link", { name: "Open sign-in page" })).toBeNull();
});
it("says why a sign-in failed and offers it again", async () => {
 h.connection = fakeConnection({ appSignInsForWorker: [signInRow({ status: "failed", url: "", userCode: "", errorMessage: "Login failed: Request failed with status code 400" })] });
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(await screen.findByText(/status code 400/)).toBeTruthy();
 expect(screen.getByRole("button", { name: "Sign in to Codex" })).toBeTruthy();
});
it("cancels a sign-in in progress", async () => {
 const c = fakeConnection({ appSignInsForWorker: [signInRow()] }, { fleetAppSignInCancel: async () => builtinReply("fleetAppSignInCancel", [{ signInId: "s1", status: "cancelled" }]) }); h.connection = c;
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 await act(async () => { fireEvent.click(await screen.findByRole("button", { name: "Cancel" })); });
 expect(c.query.fleetAppSignInCancel).toHaveBeenCalledWith({ signInId: "v1:worker:appSignIn:s1" });
});
it("says it is checking right after a sign-in, until the machine reports back", async () => {
 h.connection = fakeConnection({ appSignInsForWorker: [signInRow({ status: "succeeded", url: "", userCode: "", endedAt: now.toISOString() })] });
 render(withSession(<MachineApps machine={machine({ apps: [signedOut], appDescriptors: canSignIn })} now={now} />, { userId: "me" }));
 expect(await screen.findByText(/Checking that Codex works/)).toBeTruthy();
 expect(screen.queryByRole("button", { name: "Sign in to Codex" })).toBeNull();
});
it("reads as a status page with no copy, and offers the allow command for an app that needs it", () => {
 h.connection = fakeConnection(); render(withSession(<MachineApps machine={machine({ apps: [{ id: "codex", signedIn: true, allowed: true, subscription: "present", version: "2.1.0" }, { id: "claude-code", signedIn: true, allowed: false }] })} now={now} />, { userId: "me" }));
 const codex = within(screen.getByRole("region", { name: "Codex" }));
 expect(codex.getByText("2.1.0")).toBeTruthy();
 expect(codex.queryAllByRole("button", { name: /^Copy / })).toEqual([]);
 const claude = within(screen.getByRole("region", { name: "Claude Code" }));
 expect(claude.getByText("Allow Claude Code")).toBeTruthy();
 expect(claude.queryAllByRole("button", { name: /^Copy (Status|Resets|Last checked|Subscription|Version)$/ })).toEqual([]);
});
it("distinguishes permission, sign-in, missing inventory, offline and removed states", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: false }] }); const app = m.apps[0]!;
 expect(appStatus(m, app, now)).toBe("Permission needed");
 expect(appStatus(m, { ...app, signedIn: false }, now)).toBe("Permission needed");
 expect(appStatus(m, { ...app, allowed: true, signedIn: false }, now)).toBe("Sign-in needed");
 expect(appStatus({ ...m, appsReported: false }, undefined, now)).toBe("Not reported");
 expect(appStatus({ ...m, connectedNodeId: "" }, app, now)).toBe("Machine offline");
 expect(appStatus({ ...m, revokedAt: now.toISOString() }, app, now)).toBe("Machine removed");
});
// The engine admits only ready from a Cockpit that measures, so this surface must say
// why an app it can see is not being used, and must not offer it as runnable.
it("says a measuring machine could not check an app, and why, and does not call it runnable", () => {
 h.connection = fakeConnection();
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: true, readiness: "unknown", readinessCheckedAt: now.toISOString(), readinessDetail: "Codex is signed in with an API key" }] });
 expect(m.apps[0]!.runnable).toBe(false);
 expect(appStatus(m, m.apps[0], now)).toBe("Could not check");
 render(withSession(<MachineApps machine={m} now={now} />, { userId: "me" }));
 expect(screen.getByText("Codex is signed in with an API key")).toBeTruthy();
});
it("says a measuring machine has not checked yet, without calling it a failure", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: true, readiness: "unknown", readinessDetail: "not checked yet" }] });
 expect(m.apps[0]!.runnable).toBe(false);
 expect(appStatus(m, m.apps[0], now)).toBe("Checking");
});
it("keeps a Cockpit that predates the checks runnable, as the one exception", () => {
 const m = machine({ apps: [{ id: "codex", signedIn: true, allowed: true }] });
 expect(m.apps[0]!.runnable).toBe(true);
 expect(appStatus(m, m.apps[0], now)).toBe("Not checked");
});
it("starts one targeted harness goal, opens it, and waits for reported readiness", async () => {
 const c = fakeConnection(); h.connection = c;
 let finish!: (value: any) => void; c.query.createGoal.mockReturnValue(new Promise(resolve => { finish = resolve; }));
 render(withSession(<MachineApps machine={machine()} now={now} />, { userId: "me" }));
 const claude = within(screen.getByRole("region", { name: "Claude Code" }));
 const setup = claude.getByRole("button", { name: "Set up" }); fireEvent.click(setup); fireEvent.click(setup);
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
 expect(acts(codex)).toEqual([]);
 expect(c.query.createGoal).not.toHaveBeenCalled();
});
