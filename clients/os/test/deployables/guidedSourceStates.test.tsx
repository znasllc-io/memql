import { fireEvent, render, screen, within } from "@testing-library/react";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { builtinReply, click, fakeConnection, githubGrantRow, repositoriesReply, repositoryFixture, withSession, type FakeConnection, type FakeSeed } from "./harness";

function floor(name: string) { return within(document.querySelector(".os-actbar") as HTMLElement).queryByRole("button", { name }); }
async function open(seed: FakeSeed = {}, prepare?: (connection: FakeConnection) => void) {
  const connection = fakeConnection({ credentials: [githubGrantRow({ id: "grant", login: "alice" })], sourceConnections: [],
    githubApp: { configured: true, installUrl: "https://github.com/apps/example/installations/new" },
    repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/project" })] }), ...seed });
  prepare?.(connection); h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={new LocalDeployablesSettingsStore({getItem: () => null, setItem: () => {}})} />));
  await click(await screen.findByRole("button", { name: "Add a deployable" }));
  await click(screen.getByRole("radio", { name: /^A repository/ }));
  return connection;
}
async function organization() { await click(await screen.findByRole("button", { name: "@alice Connected" })); }
beforeEach(() => { h.connection = null; });

describe("guided source availability and recovery", () => {
  it("keeps an empty account step focused on connecting", async () => {
    await open({ credentials: [] });
    expect(screen.getByText("Connect a GitHub account to see its repositories.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add GitHub account" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Refresh GitHub accounts" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Add organization access" })).toBeNull();
    expect(floor("Continue")).toBeNull();
  });
  it("distinguishes loading access from empty access and refreshes on installation return", async () => {
    let finish!: (value: ReturnType<typeof builtinReply>) => void;
    const connection = await open({}, conn => {
      const original = vi.mocked(conn.query.executeNamed).getMockImplementation()!;
      vi.mocked(conn.query.executeNamed).mockImplementation((name, call, opts) => name === "sourceInstallations" ? new Promise(resolve => { finish = resolve; }) : original(name, call, opts));
    });
    await organization();
    expect(screen.getByText("Reading GitHub access…")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Refresh organizations" })).toBeNull();
    expect(screen.queryByText(/No approved access/)).toBeNull();
    expect(floor("Continue")).toBeNull();
    await act(async () => finish(builtinReply("sourceInstallations", [{ reason: "ok", installations: [], pending: [] }])));
    expect(screen.getByText(/No approved access yet/)).toBeTruthy();
    const link = screen.getByRole("link", { name: "Add organization access" });
    link.addEventListener("click", event => event.preventDefault(), { once: true });
    await click(link);
    act(() => { fireEvent(window, new Event("focus")); });
    expect(vi.mocked(connection.query.executeNamed).mock.calls.filter(([name]) => name === "sourceInstallations")).toHaveLength(2);
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Add GitHub account" })).toBeNull();
    await act(async () => finish(builtinReply("sourceInstallations", [{ reason: "ok", installations: [], pending: [] }])));
  });
  it("keeps an access failure recoverable without claiming there are zero organizations", async () => {
    await open({ sourceInstallationsError: "source_connection_unavailable: Access was removed." });
    await organization();
    expect(await screen.findByText("Access was removed.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Refresh organizations" })).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    expect(screen.queryByText(/No approved access/)).toBeNull();
    expect(floor("Continue")).toBeNull();
    await click(floor("Back")!);
    expect(screen.getByRole("button", { name: "Add GitHub account" })).toBeTruthy();
  });
  it("guards binding creation from double activation and advances after scope validation", async () => {
    let finish!: () => void;
    const pending = new Promise<void>(resolve => { finish = resolve; });
    const connection = await open({}, conn => {
      const original = vi.mocked(conn.query.executeNamed).getMockImplementation()!;
      vi.mocked(conn.query.executeNamed).mockImplementation(async (name, call, opts) => { const answer = await original(name, call, opts); if (name === "sourceConnectionCreate") await pending; return answer; });
    });
    await organization();
    const row = await screen.findByRole("button", { name: "acme Organization" });
    act(() => { row.click(); row.click(); });
    expect(screen.getByText("Checking organization access…")).toBeTruthy();
    expect(floor("Continue")).toBeNull();
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(1);
    expect(floor("Back")).toBeTruthy();
    expect(screen.getByRole("button", { name: /acme Organization Checking access/ }).hasAttribute("disabled")).toBe(true);
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    await act(async () => finish());
    await screen.findByRole("button", { name: /^project main/ });
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(1);
  });
  it("shows repository failures with a retry at the top and no configuration fields", async () => {
    await open({ repositoriesError: "repository_not_accessible: Permission was removed." });
    await organization();
    await click(await screen.findByRole("button", { name: "acme Organization" }));
    expect(await screen.findByText("Permission was removed.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Refresh repositories" })).toBeTruthy();
    expect(screen.queryByLabelText("Accounts")).toBeNull();
    expect(screen.queryByRole("link", { name: "Add organization access" })).toBeNull();
    expect(floor("Continue")).toBeNull();
  });
});
