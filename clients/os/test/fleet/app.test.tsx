import { act, render, screen, waitFor } from "@testing-library/react";
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
const { appById, sectionsForRole } = await import("../../src/system/registry");
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
  it("declares the epic's four sections, Machines first, with a settings gear target", () => {
    const fleet = appById(OS_REGISTRY, "fleet");
    expect(fleet).toBeTruthy();
    expect(sectionsForRole(fleet!, "owner").map((s) => s.id)).toEqual([
      "machines",
      "routing",
      "workbenches",
      "settings",
    ]);
    // The gear has somewhere to go, and the settings picker offers exactly
    // the sections the manifest declares -- a preference naming one it does
    // not would leave the window on Machines with the nav highlighting
    // nothing.
    expect(fleet!.settingsSection).toBe("settings");
    expect(FLEET_SECTION_IDS).toEqual(sectionsForRole(fleet!, "owner").map((s) => s.id));
  });

  it("admits every signed-in user: the engine's row tiers decide what comes back", () => {
    const fleet = appById(OS_REGISTRY, "fleet")!;
    expect(fleet.roles).toBeUndefined();
    expect(sectionsForRole(fleet, "reader").map((s) => s.id)).toHaveLength(4);
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
    expect(await screen.findByRole("heading", { name: "Routing" })).toBeTruthy();
    second.view.unmount();

    const third = mount(fakeConnection(), "workbenches", store);
    expect(await screen.findByRole("heading", { name: "Workbenches" })).toBeTruthy();
    third.view.unmount();

    mount(fakeConnection(), "settings", store);
    expect(await screen.findByRole("heading", { name: "Fleet settings" })).toBeTruthy();
  });

  it("navigates to the stored default section once, on open", async () => {
    const store = memoryStore({ version: 1, defaultSection: "workbenches", showRevoked: false });
    const { navigate } = mount(fakeConnection(), "machines", store);

    await waitFor(() => expect(navigate).toHaveBeenCalledWith("workbenches"));
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("does not drag an operator back after they navigate away themselves", async () => {
    const store = memoryStore({ version: 1, defaultSection: "workbenches", showRevoked: false });
    const { view, navigate } = mount(fakeConnection(), "machines", store);
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
    await waitFor(() => expect(screen.getByRole("heading", { name: "Routing" })).toBeTruthy());
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
      defaultSection: "machines",
      showRevoked: true,
    });

    // The same store, a fresh window: the preference is what the list reads.
    view.unmount();
    mount(fakeConnection({ myWorkersWithStatus: [REVOKED] }), "machines", store);
    expect(await screen.findByText("Old laptop")).toBeTruthy();
  });

  it("stores the chosen default section", async () => {
    const store = memoryStore(DEFAULT_FLEET_SETTINGS);
    mount(fakeConnection(), "settings", store);
    await click(await screen.findByRole("radio", { name: "Routing" }));
    expect(store.saved.at(-1)?.defaultSection).toBe("routing");
  });
});
