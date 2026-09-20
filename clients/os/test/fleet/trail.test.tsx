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

// THE REGRESSION THIS PINS. Every machine view published its depth except the
// one a machine opens on: Equipment read "Machines" while Details, one click
// away, read "Machines > Alpha > Details" -- as though selecting a machine had
// not happened until you left its first page.
it("names the machine and the view on Equipment, the same as on every other view", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await screen.findByRole("button", { name: /Machine details/ });
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));

  fireEvent.click(screen.getByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Details"]));
});

it("never draws a second trail, whichever machine view is open", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await screen.findByRole("button", { name: /Machine details/ });
  expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs().at(-1)).toBe("Details"));
  expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
  expect(screen.getAllByRole("button", { name: /^Back/ })).toHaveLength(1);
});

it("does not link the ancestors on Equipment, which lead to where the person already is", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));
  const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
  expect(within(path).queryAllByRole("button")).toHaveLength(0);
  // Nowhere to go back to from a machine's home, so Back is in place and inert.
  expect((screen.getByRole("button", { name: "Back" }) as HTMLButtonElement).disabled).toBe(true);
});

it("links them again one level down, and Back returns to the machine", async () => {
  h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] });
  fleet();
  fireEvent.click(await screen.findByRole("button", { name: /Machine details/ }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Details"]));
  fireEvent.click(screen.getByRole("button", { name: "Back to Alpha" }));
  await waitFor(() => expect(crumbs()).toEqual(["Machines", "Alpha", "Equipment"]));
});

it("reads just Machines when no machine is registered", async () => {
  fleet();
  await waitFor(() => expect(crumbs()).toEqual(["Machines"]));
});
