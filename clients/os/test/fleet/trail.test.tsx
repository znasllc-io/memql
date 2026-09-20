import { useRef, type ReactNode } from "react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, machineRow, withSession } = await import("./harness");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { MachinesProvider } = await import("../../src/live/machines");
const { PageNavigationProvider } = await import("../../src/kit/pageNavigation");
const { TrailRow } = await import("../../src/kit/TrailRow");
const { installSeededAccess } = await import("../seededAccess");

afterEach(cleanup);
beforeEach(() => { installSeededAccess("owner"); h.connection = fakeConnection(); });

// The window frame draws the one trail row and the app publishes to it. This
// mounts Fleet the way the frame does, so the trail is the real one.
function Frame({ children }: { children: ReactNode }) {
  const root = useRef<HTMLDivElement>(null);
  return <div ref={root}><PageNavigationProvider root={root} trail={[]}>
    <TrailRow fallback="Machines" />
    {children}
  </PageNavigationProvider></div>;
}

function fleet() {
  return render(withSession(<MachinesProvider><Frame><FleetApp sectionId="machines" navigate={vi.fn()} askContext={vi.fn()} store={{ load: () => ({ version: 1, defaultSection: "machines", showRevoked: false }), save: () => {} }} /></Frame></MachinesProvider>));
}

const crumbs = () => within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map(item => item.textContent);

// A machine is OPENED FROM THE LIST now. The section used to select the first
// machine on sight, so it was always inside one and a dropdown was the only
// way to another; an empty selection is the list, and a row opens a machine.
const openMachine = async (name: string) => fireEvent.click(await screen.findByRole("button", { name: new RegExp(`^Open ${name}`) }));

it("opens on the list of machines, not inside the first one", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" }), machineRow({ id: "b", displayName: "Beta" })] });
  fleet();
  expect(await screen.findByRole("button", { name: /^Open Alpha/ })).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Open Beta/ })).toBeTruthy();
  await waitFor(() => expect(crumbs()).toEqual(["Machines"]));
  // Nothing is selected, so there is no machine page and nowhere to go back to.
  expect(screen.queryByRole("navigation", { name: "Machine views" })).toBeNull();
  expect((screen.getByRole("button", { name: "Back" }) as HTMLButtonElement).disabled).toBe(true);
});

it("has no machine dropdown: the list is the one way to pick a machine", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await screen.findByRole("button", { name: /^Open Alpha/ });
  expect(screen.queryByRole("combobox", { name: "Selected machine" })).toBeNull();
  await openMachine("Alpha");
  await screen.findByRole("navigation", { name: "Machine views" });
  expect(screen.queryByRole("combobox", { name: "Selected machine" })).toBeNull();
});

it("keeps Add a machine on the list", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await screen.findByRole("button", { name: /^Open Alpha/ });
  expect(screen.getByRole("button", { name: "Add a machine" })).toBeTruthy();
});

// THE REGRESSION THIS PINS. Every machine view published its depth except the
// one a machine opens on: Equipment read "Machines" while Details, one click
// away, read "Machines > Alpha > Details" -- as though selecting a machine had
// not happened until you left its first page.
it("names the machine and the view on Equipment, the same as on every other view", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await openMachine("Alpha");
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));

  fireEvent.click(screen.getByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Details"]));
});

it("never draws a second trail, whichever machine view is open", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await openMachine("Alpha");
  await screen.findByRole("button", { name: /Machine details/ });
  expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs().at(-1)).toBe("Details"));
  expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
  expect(screen.getAllByRole("button", { name: /^Back/ })).toHaveLength(1);
});

it("makes Machines a link home from a machine, and leaves the machine's own crumb plain on its home page", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await openMachine("Alpha");
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));
  const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
  // "Machines" goes somewhere now -- there is a list behind it. "Alpha" leads
  // to this very page, and a crumb that goes nowhere is not a link.
  expect(within(path).getByRole("button", { name: "Machines" })).toBeTruthy();
  expect(within(path).queryByRole("button", { name: "Alpha" })).toBeNull();
});

it("goes Back from a machine to the list, and from a view to the machine", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await openMachine("Alpha");
  fireEvent.click(await screen.findByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Details"]));

  fireEvent.click(screen.getByRole("button", { name: "Back to Alpha" }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));

  fireEvent.click(screen.getByRole("button", { name: "Back to Machines" }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines"]));
  expect(await screen.findByRole("button", { name: /^Open Alpha/ })).toBeTruthy();
});

it("returns to the list from the Machines crumb", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await openMachine("Alpha");
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));
  fireEvent.click(within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getByRole("button", { name: "Machines" }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines"]));
});

it("reads just Machines when no machine is registered", async () => {
  fleet();
  await waitFor(() => expect(crumbs()).toEqual(["Machines"]));
});
