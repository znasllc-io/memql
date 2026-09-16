import { StrictMode, type ReactNode } from "react";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Result } from "@znasllc-io/memql-sdk-core/client";

// Only the wire is substituted. The real Shell owns capability loading,
// dispatch, GraphDesktopStore hydration and the visible Deployables window.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: ReactNode }) => children,
  useOsConnection: () => h.connection,
}));
import { Shell } from "../../src/chrome/Shell";
import { StubAskTransport } from "../../src/ask/askController";
import { captureConnectReturn, clearParkedConnectReturn, takeParkedConnectReturn } from "../../src/apps/deployables/sources/connectReturn";
import { clearEffectiveCapabilities } from "../../src/system/roles";
import { LocalDesktopStore, type DesktopDocument } from "../../src/system/store";
import { seededAccessFor, installSeededAccess } from "../seededAccess";
import { builtinReply, fakeConnection, githubGrantRow, repositoriesReply, repositoryFixture, rowsResult } from "./harness";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
const documentFromOtherDevice: DesktopDocument = {
  version: 1,
  desks: [{ id: "remote-desk", createdBy: "user" }],
  activeDeskId: "remote-desk",
  surfaces: { "remote-desk": { items: {}, positions: {} } },
  dock: { pinned: ["settings"] },
  themePack: "midnight",
  installedPacks: [],
};
function boot() {
  const capabilities = deferred<Result>();
  const desktop = deferred<Result>();
  const connection = fakeConnection({
    credentials: [githubGrantRow({ id: "own-grant" })],
    repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/private-site" })] }),
  });
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, options) => {
    if (name === "effectiveCapabilitiesForActor") return capabilities.promise;
    if (name === "myDesktop") return desktop.promise;
    return original(name, call, options);
  });
  history.replaceState({}, "", "/?connect=deployables&github=reconnected");
  captureConnectReturn(window);
  const shell = () => <StrictMode><Shell layout="desktop"
    access={{ userId: "u-me", primaryEmail: "owner@example.test", role: "owner", roleName: "", rank: 0 }}
    config={{ identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "client", authEnabled: true, domain: "example.test" }}
    onSignOut={vi.fn()}
    ports={{ disableConnection: true, askTransport: new StubAskTransport(), askVoice: null }}
  /></StrictMode>;
  // Dial after mounting: the real local store changes to GraphDesktopStore.
  const view = render(shell());
  h.connection = connection;
  view.rerender(shell());
  return { capabilities, desktop, connection };
}
beforeEach(() => {
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null);
  h.connection = null;
  localStorage.clear();
  clearParkedConnectReturn();
  clearEffectiveCapabilities();
});
afterEach(() => {
  cleanup();
  clearParkedConnectReturn();
  installSeededAccess("owner");
  h.connection = null;
});
async function allow(capabilities: ReturnType<typeof deferred<Result>>) {
  await act(async () => capabilities.resolve(builtinReply("effectiveCapabilitiesForActor", [{ ok: true, entries: seededAccessFor("owner") }])));
}
async function hydrate(desktop: ReturnType<typeof deferred<Result>>) {
  await act(async () => desktop.resolve(rowsResult([{ revision: 4, document: documentFromOtherDevice }])));
}
describe("GitHub callback through real shell startup", () => {
  it("waits for capabilities after desktop hydration, then opens one repository picker", async () => {
    const { capabilities, desktop, connection } = boot();
    await hydrate(desktop);
    expect(screen.queryByRole("dialog", { name: "Deployables" })).toBeNull();
    await allow(capabilities);
    expect(await screen.findByRole("button", { name: /private-site/ })).toBeTruthy();
    expect(screen.getAllByRole("dialog", { name: "Deployables" })).toHaveLength(1);
    expect(new LocalDesktopStore().load()?.desks).toHaveLength(1);
    expect(takeParkedConnectReturn()).toBeNull();
    expect(window.location.search).toBe("");
    expect(connection.callsNamed("sourceRepositories").every(call => call.includes("own-grant"))).toBe(true);
  });
  it("keeps the returned window and component state when the desktop hydrates later", async () => {
    const { capabilities, desktop } = boot();
    await allow(capabilities);
    const repository = await screen.findByRole("button", { name: /private-site/ });
    const originalWindow = screen.getByRole("dialog", { name: "Deployables" });
    await hydrate(desktop);
    await waitFor(() => expect(new LocalDesktopStore().load()?.desks.some(d => d.id === "remote-desk")).toBe(true));
    expect(screen.getByRole("dialog", { name: "Deployables" })).toBe(originalWindow);
    expect(screen.getByRole("button", { name: /private-site/ })).toBe(repository);
    expect(screen.getAllByRole("dialog", { name: "Deployables" })).toHaveLength(1);
    const persisted = new LocalDesktopStore().load()!;
    expect(persisted.desks).toHaveLength(2);
    expect(persisted.dock.pinned).toEqual(["settings"]);
    expect(persisted.themePack).toBe("midnight");
    expect(JSON.stringify(persisted)).not.toContain("own-grant");
    expect(JSON.stringify(persisted)).not.toContain("reconnected");
  });
  it("does not consume or open a callback when access is denied", async () => {
    const { capabilities, desktop } = boot();
    await hydrate(desktop);
    await act(async () => capabilities.resolve(builtinReply("effectiveCapabilitiesForActor", [{ ok: true, entries: [] }])));
    expect(screen.queryByRole("dialog", { name: "Deployables" })).toBeNull();
    expect(takeParkedConnectReturn()).toEqual({ section: "deployables", reason: "reconnected" });
  });
});
