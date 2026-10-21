import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

// A machine tab's page title keeps the same space above its first section,
// including the titles the Models and Activity tabs draw inside their own
// content. jsdom lays nothing out, so this holds the two halves the browser
// combines: each title sits in the inspector, and the inspector's rule spaces
// every title in it (measured in a browser: 8px before, 28px after).

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, machineRow, withSession } = await import("./harness");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { MachinesProvider } = await import("../../src/live/machines");
const { installSeededAccess } = await import("../seededAccess");

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

afterEach(cleanup);
beforeEach(() => { installSeededAccess("owner"); h.connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "a", displayName: "Alpha" })] }); });

it("spaces every machine tab title, including the ones Models and Activity draw", async () => {
  render(withSession(<MachinesProvider><FleetApp sectionId="machines" navigate={vi.fn()} askContext={vi.fn()} store={{ load: () => ({ version: 1, defaultSection: "machines", showRevoked: false }), save: () => {} }} /></MachinesProvider>));
  fireEvent.click(await screen.findByRole("button", { name: /^Open Alpha/ }));
  const tabs = screen.getByRole("navigation", { name: "Machine views" });
  for (const tab of ["Models", "Activity", "Apps"]) {
    fireEvent.click(within(tabs).getByRole("button", { name: tab }));
    const title = (await screen.findAllByRole("heading", { name: tab })).find(heading => heading.closest(".os-head") && !heading.closest("[hidden]"));
    expect(title?.closest(".fleet-inspector")).toBeTruthy();
  }
  expect(CSS).toContain(".fleet-inspector .os-head { margin-bottom: 20px; }");
  expect(CSS).not.toContain(".fleet-inspector > .os-head { margin-bottom");
});
