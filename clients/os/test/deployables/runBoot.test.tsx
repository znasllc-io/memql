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
import { StubAskTransport } from "../ask/stubTransport";
import { captureRunOpen, resetParkedRunOpenForTest, takeParkedRunOpen } from "../../src/apps/deployables/pipelines/openRun";
import { clearEffectiveCapabilities } from "../../src/system/roles";
import { LocalDesktopStore, type DesktopDocument } from "../../src/system/store";
import { seededAccessFor, seededAccessWithout, installSeededAccess } from "../seededAccess";
import { builtinReply, fakeConnection, rowsResult } from "./harness";

import { packageRow, pipelineRow, runRow, VIEWER } from "./pipelines/fixtures";

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
    packages: [packageRow()],
    pipelines: [pipelineRow()],
    pipelineRuns: [runRow({ id: "run-linked", ownerUserId: VIEWER, title: "The linked pipeline run", stages: [], workRunId: "" })],
  });
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, options) => {
    if (name === "effectiveCapabilitiesForActor") return capabilities.promise;
    if (name === "myDesktop") return desktop.promise;
    return original(name, call, options);
  });
  history.replaceState({}, "", "/?pipelineRun=run-linked&keep=1");
  captureRunOpen(window);
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
  resetParkedRunOpenForTest();
  clearEffectiveCapabilities();
});
afterEach(() => {
  cleanup();
  resetParkedRunOpenForTest();
  installSeededAccess("owner");
  h.connection = null;
});
async function allow(capabilities: ReturnType<typeof deferred<Result>>) {
  await act(async () => capabilities.resolve(builtinReply("effectiveCapabilitiesForActor", [{ ok: true, entries: seededAccessFor("owner") }])));
}
async function hydrate(desktop: ReturnType<typeof deferred<Result>>) {
  await act(async () => desktop.resolve(rowsResult([{ revision: 4, document: documentFromOtherDevice }])));
}
describe("GitHub check details through real shell startup", () => {
  it.each(["desktop first", "capabilities first"])("opens the requested run once with %s", async order => {
    const { capabilities, desktop } = boot();
    expect(screen.queryByRole("dialog", { name: "Deployables" })).toBeNull();
    if (order === "desktop first") await hydrate(desktop);
    await allow(capabilities);
    const heading = await screen.findByRole("heading", { name: "The linked pipeline run" });
    const originalWindow = screen.getByRole("dialog", { name: "Deployables" });
    if (order === "capabilities first") await hydrate(desktop);
    await waitFor(() => expect(new LocalDesktopStore().load()?.desks.some(d => d.id === "remote-desk")).toBe(true));
    expect(screen.getByRole("heading", { name: "The linked pipeline run" })).toBe(heading);
    expect(screen.getByRole("dialog", { name: "Deployables" })).toBe(originalWindow);
    expect(screen.getAllByRole("dialog", { name: "Deployables" })).toHaveLength(1);
    expect(takeParkedRunOpen()).toBeNull();
    expect(window.location.search).toBe("?keep=1");
    expect(JSON.stringify(new LocalDesktopStore().load())).not.toContain("run-linked");
  });
  it.each(["app:deployables", "app:deployables/runs"])("keeps the intent parked when %s is denied", async resource => {
    const { capabilities, desktop } = boot();
    await hydrate(desktop);
    await act(async () => capabilities.resolve(builtinReply("effectiveCapabilitiesForActor", [{ ok: true, entries: seededAccessWithout("owner", resource) }])));
    expect(screen.queryByRole("dialog", { name: "Deployables" })).toBeNull();
    expect(takeParkedRunOpen()).toBe("run-linked");
  });
});
