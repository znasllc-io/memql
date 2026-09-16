import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// Type-only, so it is erased before the mock factories run.
import type { FleetSettings, FleetSettingsStore } from "../../src/apps/fleet/settings";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MachinesProvider } = await import("../../src/live/machines");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { appById, sectionsFor } = await import("../../src/system/registry");
const { installSeededAccess } = await import("../seededAccess");
const { DEFAULT_FLEET_SETTINGS, FLEET_SECTION_IDS } = await import(
  "../../src/apps/fleet/settings"
);
const { fakeConnection, machineRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function memoryStore(initial: FleetSettings): FleetSettingsStore & { saved: FleetSettings[] } {
  let held = initial;
  const saved: FleetSettings[] = [];
  return {
    saved,
    load: () => held,
    save: (next) => {
      held = next;
      saved.push(next);
    },
  };
}

function mount(
  connection: Conn,
  sectionId: string,
  store: FleetSettingsStore,
  navigate = vi.fn(),
) {
  h.connection = connection;
  const view = render(
    withSession(
      <MachinesProvider>
        <FleetApp
          sectionId={sectionId}
          navigate={navigate}
          askContext={vi.fn()}
          store={store}
        />
      </MachinesProvider>,
    ),
  );
  return { view, navigate };
}

const REVOKED = machineRow({
  id: "v1:worker:registration:gone",
  displayName: "Old laptop",
  revokedAt: "2026-08-30T11:00:00Z",
});

beforeEach(() => {
  h.connection = null;
});

describe("the Fleet manifest", () => {
  it("declares its sections in order, Overview first, with a settings gear target", () => {
    const fleet = appById(OS_REGISTRY, "fleet");
    expect(fleet).toBeTruthy();
    // Models sits between Machines and Routing (epic memql#5096): the
    // hardware, then what runs on it, then how calls are steered to it -- and
    // Routing's model preference reorders the ranking Models shows. Apps sits
    // after Workbenches (epic memql#5009): the cluster's own sandbox, then the
    // person's own computer, then the logs about both.
    installSeededAccess("owner");
    expect(sectionsFor(fleet!).map((s) => s.id)).toEqual([
      "overview",
      "machines",
      "policies",
      "models",
      "routing",
      "apps",
      "workbenches",
      "logs",
      "settings",
    ]);
    // The gear has somewhere to go, and the settings picker offers exactly
    // the sections the manifest declares -- a preference naming one it does
    // not would leave the window on Machines with the nav highlighting
    // nothing.
    expect(fleet!.settingsSection).toBe("settings");
    expect(FLEET_SECTION_IDS).toEqual(sectionsFor(fleet!).map((s) => s.id));
  });

  it("admits every signed-in user: the engine's row tiers decide what comes back", () => {
    const fleet = appById(OS_REGISTRY, "fleet")!;
    expect(fleet.requires).toBe("app:fleet");
    // Everything but Logs for a reader: that is the one section floored at
    // admin (epic memql#4895), because every read on the log store is. Apps
    // is NOT floored -- both concepts behind it declare the composite owner
    // tier -- and neither is Models, whose two readings are caller-scoped
    // projections of the reader's own fleet (epic memql#5096).
    installSeededAccess("reader");
    expect(sectionsFor(fleet).map((s) => s.id)).toEqual([
      "overview",
      "machines",
      "policies",
      "models",
      "routing",
      "apps",
      "workbenches",
      "settings",
    ]);
  });
});

describe("the Fleet app shell", () => {
  it("routes each section to its own surface", async () => {
    const store = memoryStore(DEFAULT_FLEET_SETTINGS);
    const connection = fakeConnection();

    const first = mount(connection, "machines", store);
    expect(await screen.findByRole("heading", { name: "Machines" })).toBeTruthy();
    first.view.unmount();

    const second = mount(fakeConnection(), "routing", store);
    expect(await screen.findByRole("heading", { name: "Machine routing" })).toBeTruthy();
    second.view.unmount();

    const third = mount(fakeConnection(), "workbenches", store);
    expect(await screen.findByRole("heading", { name: "Cluster workspaces" })).toBeTruthy();
    third.view.unmount();

    const fourth = mount(fakeConnection(), "apps", store);
    expect(await screen.findByRole("heading", { name: "Activity" })).toBeTruthy();
    fourth.view.unmount();

    mount(fakeConnection(), "settings", store);
    expect(await screen.findByRole("heading", { name: "Fleet settings" })).toBeTruthy();
  });

  it("navigates to the stored default section once, on open", async () => {
    const store = memoryStore({ version: 1, defaultSection: "workbenches", showRevoked: false });
    const { navigate } = mount(fakeConnection(), "overview", store);

    await waitFor(() => expect(navigate).toHaveBeenCalledWith("workbenches"));
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("does not drag an operator back after they navigate away themselves", async () => {
    const store = memoryStore({ version: 1, defaultSection: "workbenches", showRevoked: false });
    const { view, navigate } = mount(fakeConnection(), "overview", store);
    await waitFor(() => expect(navigate).toHaveBeenCalledTimes(1));

    // The window navigates; the app component stays mounted and only its
    // props change, which is what makes the once-per-window guard correct.
    view.rerender(
      withSession(
        <MachinesProvider>
          <FleetApp sectionId="routing" navigate={navigate} askContext={vi.fn()} store={store} />
        </MachinesProvider>,
      ),
    );
    await waitFor(() => expect(screen.getByRole("heading", { name: "Machine routing" })).toBeTruthy());
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("navigates nowhere when the default is already the section the window opened on", async () => {
    const store = memoryStore(DEFAULT_FLEET_SETTINGS);
    const { navigate } = mount(fakeConnection(), "machines", store);
    await screen.findByRole("heading", { name: "Machines" });
    expect(navigate).not.toHaveBeenCalled();
  });

  it("persists the settings and applies show-revoked to the machines list", async () => {
    const store = memoryStore(DEFAULT_FLEET_SETTINGS);
    const connection = fakeConnection({ myWorkersWithStatus: [REVOKED] });
    const { view } = mount(connection, "settings", store);

    await click(await screen.findByLabelText("List revoked machines"));
    expect(store.saved.at(-1)).toEqual({
      version: 1,
      defaultSection: "overview",
      showRevoked: true,
    });

    // The same store, a fresh window: the preference is what the list reads.
    view.unmount();
    mount(fakeConnection({ myWorkersWithStatus: [REVOKED] }), "machines", store);
    expect((await screen.findAllByText("Old laptop"))[0]).toBeTruthy();
  });

  it("stores the chosen default section", async () => {
    const store = memoryStore(DEFAULT_FLEET_SETTINGS);
    mount(fakeConnection(), "settings", store);
    await click(await screen.findByRole("radio", { name: "Machine routing" }));
    expect(store.saved.at(-1)?.defaultSection).toBe("routing");
  });
});


describe("Fleet overview", () => {
  it("counts only current registrations and current online activity", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [
      machineRow({ id: "online", displayName: "Studio", activeCount: 3 }),
      machineRow({ id: "offline", displayName: "Laptop", connectedNodeId: "", activeCount: 8 }),
      machineRow({ id: "stale", displayName: "Older heartbeat", lastSeenAt: "2000-01-01T00:00:00Z", activeCount: 9 }),
      REVOKED,
    ] });
    const { navigate } = mount(connection, "overview", memoryStore(DEFAULT_FLEET_SETTINGS));
    await screen.findByRole("button", { name: "Open Studio, online" });
    const value = (label: string) => document.querySelector(`[data-overview-metric="${label}"] dd`)?.textContent;
    expect(value("Machines")).toBe("3");
    expect(value("Online")).toBe("1");
    expect(value("Active calls")).toBe("3");
    expect(screen.queryByRole("button", { name: /Old laptop/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open Studio, online" }));
    expect(navigate).toHaveBeenCalledWith("machines", { fromContent: true });
    expect(connection.query.myWorkersWithStatus).toHaveBeenCalledTimes(1);
  });

  it("does not present loading as an empty or idle fleet", async () => {
    const connection = fakeConnection();
    connection.query.myWorkersWithStatus.mockReturnValue(new Promise(() => {}));
    mount(connection, "overview", memoryStore(DEFAULT_FLEET_SETTINGS));
    expect(screen.getByRole("heading", { name: "Reading your fleet" })).toBeTruthy();
    expect(document.querySelector('[data-overview-metric="Machines"] dd')?.textContent).toBe("—");
    expect(document.querySelector('[data-overview-metric="Active calls"] dd')?.textContent).toBe("—");
  });
});
