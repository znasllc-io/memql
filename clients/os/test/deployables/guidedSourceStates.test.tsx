import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, githubGrantRow, repositoriesReply, repositoryFixture, withSession, type FakeSeed } from "./harness";

async function open(seed: FakeSeed = {}) {
  const connection = fakeConnection({ credentials: [githubGrantRow({ id: "grant", login: "alice" })],
    repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/project" })] }), ...seed });
  h.connection = connection;
  const navigate = vi.fn();
  render(withSession(<DeployablesApp sectionId="deployables" navigate={navigate} askContext={vi.fn()} store={new LocalDeployablesSettingsStore({getItem: () => null, setItem: () => {}})} />));
  await click(await screen.findByRole("button", { name: "Add a deployable" }));
  return { connection, navigate };
}
beforeEach(() => { h.connection = null; });

describe("repository setup belongs to Settings", () => {
  it.each([{ credentials: [] }, { sourceConnections: [] }, { credentials: [githubGrantRow({ id: "revoked", status: "revoked" })] }])("marks incomplete setup and navigates to Settings", async seed => {
    const { connection, navigate } = await open(seed);
    expect(await screen.findByRole("img", { name: "GitHub setup needed" })).toBeTruthy();
    await click(screen.getByRole("radio", { name: /^A repository/ }));
    expect(navigate).toHaveBeenCalledWith("settings", { fromContent: true });
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(0);
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(screen.getByRole("radio", { name: /^A zip in Files/ })).toBeTruthy();
  });
  it("selects saved account, organization and repository without registering access", async () => {
    const { connection, navigate } = await open();
    expect(screen.queryByRole("img", { name: "GitHub setup needed" })).toBeNull();
    await click(screen.getByRole("radio", { name: /^A repository/ }));
    expect(screen.queryByRole("button", { name: "Add GitHub account" })).toBeNull();
    await click(screen.getByRole("button", { name: "Add or manage GitHub accounts and organizations in Settings" }));
    expect(navigate).toHaveBeenCalledWith("settings", { fromContent: true });
    await click(await screen.findByRole("button", { name: "@alice Connected" }));
    expect(screen.queryByRole("link", { name: "Add organization access" })).toBeNull();
    await click(await screen.findByRole("button", { name: "acme Organization" }));
    await click(await screen.findByRole("button", { name: /^project main/ }));
    expect(await screen.findByLabelText("Accounts")).toBeTruthy();
    expect(connection.callsNamed("sourceInstallations")).toHaveLength(0);
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(connection.callsNamed("sourceRepositories")[0]).toContain('connectionId: "source-grant-i-acme"');
  });
  it("keeps a failed organization read recoverable instead of reporting missing setup", async () => {
    await open({ sourceConnectionsError: "Connection read failed" });
    await click(screen.getByRole("radio", { name: /^A repository/ }));
    expect(screen.queryByRole("img", { name: "GitHub setup needed" })).toBeNull();
    await click(await screen.findByRole("button", { name: "@alice Connected" }));
    expect(await screen.findByText("Connection read failed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    expect(screen.queryByText("No organizations connected.")).toBeNull();
  });
  it("keeps repository errors visible with recovery and Settings access", async () => {
    await open({ repositoriesError: "repository_not_accessible: Permission was removed." });
    await click(screen.getByRole("radio", { name: /^A repository/ }));
    await click(await screen.findByRole("button", { name: "@alice Connected" }));
    await click(await screen.findByRole("button", { name: "acme Organization" }));
    expect(await screen.findByText("Permission was removed.")).toBeTruthy();
    expect(screen.queryByLabelText("Accounts")).toBeNull();
    expect(screen.getByRole("button", { name: "Add or manage GitHub accounts and organizations in Settings" })).toBeTruthy();
    expect(within(document.querySelector(".os-actbar")!).queryByRole("button", { name: "Continue" })).toBeNull();
  });
});
