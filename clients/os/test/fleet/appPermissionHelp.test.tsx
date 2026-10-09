import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AppPermissionHelp } from "../../src/apps/fleet/machines/AppPermissionHelp";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineRow, withSession } from "./harness";

afterEach(cleanup);
const machine = machineFromRow(machineRow({ id: "pop", displayName: "Pop!_OS", version: "0.17.0+abc123", apps: [{ id: "claude-code", allowed: false, signedIn: true }] }));
const app = machine.apps[0]!;

it("copies a cluster-scoped grant without claiming to enable the app", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const view = render(withSession(<AppPermissionHelp app={app} machine={machine} />, { domain: "memql.localhost" }));
  fireEvent.click(screen.getByText("Allow Claude Code"));
  fireEvent.click(screen.getByRole("button", { name: "Copy the command to allow Claude Code for this cluster" }));
  expect(writeText).toHaveBeenCalledWith("memql worker apps --allow claude-code --home 'https://api.memql.localhost'");
  expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
  expect(screen.getByText("Allow Claude Code")).toBeTruthy();
  view.rerender(withSession(<AppPermissionHelp app={{ ...app, allowed: true }} machine={machine} />));
  expect(screen.queryByText("Allow Claude Code")).toBeNull();
});

it("names the required update for the installed 0.16 release and the separate app sign-in", () => {
  render(withSession(<AppPermissionHelp app={{ ...app, signedIn: false }} machine={{ ...machine, version: "0.16.0" }} />));
  expect(screen.getByText(/Update Cockpit to 0.17.0/).textContent).toContain("0.16.0");
  expect(screen.getByText(/Then sign in to Claude Code/)).toBeTruthy();
});

it("offers no grant for an unknown app or revoked machine", () => {
  const view = render(withSession(<AppPermissionHelp app={{ ...app, id: "untrusted; app" }} machine={machine} />));
  expect(screen.queryByRole("textbox")).toBeNull();
  view.rerender(withSession(<AppPermissionHelp app={app} machine={{ ...machine, revokedAt: new Date().toISOString() }} />));
  expect(screen.queryByRole("textbox")).toBeNull();
});

it("does not guess a home when the cluster address is unavailable", () => {
  render(withSession(<AppPermissionHelp app={app} machine={machine} />, { domain: "" }));
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(screen.getByText(/cluster address is unavailable/)).toBeTruthy();
});

it("keeps shell syntax in a deployment value inside the quoted home argument", () => {
  render(withSession(<AppPermissionHelp app={{ ...app, id: "codex" }} machine={machine} />, { domain: "test'$(echo unsafe)" }));
  expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("memql worker apps --allow codex --home 'https://api.test'\\''$(echo unsafe)'");
});
