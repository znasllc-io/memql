import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { resetIdsForTest } from "../../src/system/desks";
import { ADMIN, READER, gotoSection, openFromLauncher, renderShell } from "../settings/shellHarness";

// The Logs section and the Logs app through the REAL shell (epic memql#4895):
// the nav offers the section to an admin and hides it from a reader, and the
// launcher does the same with the app. `sectionsForRole` is pinned over the
// registry elsewhere; this is the nav actually drawing the answer.

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
});

function navNames(app: string): string[] {
  const nav = screen.getByRole("navigation", { name: `${app} sections` });
  return within(nav)
    .getAllByRole("button")
    .map((button) => button.textContent ?? "");
}

describe("the Logs section in a window's nav", () => {
  it("an admin's Fleet window offers Logs, right before Settings", () => {
    renderShell({ access: ADMIN });
    openFromLauncher("Fleet");
    // Apps joined between Workbenches and Logs (epic memql#5009); Logs is
    // still the section immediately before Settings, which is what this
    // pins.
    expect(navNames("Fleet")).toEqual([
      "Machines",
      "Routing",
      "Workbenches",
      "Apps",
      "Logs",
      "Settings",
    ]);
  });

  it("a reader's Fleet window does not", () => {
    renderShell({ access: READER });
    openFromLauncher("Fleet");
    // Apps is NOT admin-floored -- both concepts behind it declare the
    // composite owner tier -- so a reader keeps it and loses only Logs.
    expect(navNames("Fleet")).toEqual([
      "Machines",
      "Routing",
      "Workbenches",
      "Apps",
      "Settings",
    ]);
  });

  it("opening the section renders it, and it says the shell is not connected", () => {
    renderShell({ access: ADMIN });
    openFromLauncher("Fleet");
    gotoSection("Fleet", "Logs");
    const window = document.querySelector("[data-os-window='fleet']") as HTMLElement;
    expect(within(window).getByRole("heading", { name: "Logs" })).toBeTruthy();
    // The harness dials nothing, and the section says so in surface rather
    // than rendering an empty list that reads as "nothing logged".
    expect(within(window).getByText("Not connected to the cluster.")).toBeTruthy();
  });
});

describe("the Logs app in the launcher", () => {
  it("is offered to an admin and opens on Stream", () => {
    renderShell({ access: ADMIN });
    openFromLauncher("Logs");
    expect(document.querySelector("[data-os-window='logs']")).not.toBeNull();
    expect(navNames("Logs")).toEqual(["Stream", "Search", "Settings"]);
    expect(screen.getByRole("heading", { name: "Stream" })).toBeTruthy();
  });

  it("is absent from a reader's launcher", () => {
    renderShell({ access: READER });
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = screen.getByRole("dialog", { name: "Launcher" });
    expect(within(launcher).queryByRole("button", { name: "Logs" })).toBeNull();
    // The reachable positive: the launcher did draw apps for this reader.
    expect(within(launcher).getByRole("button", { name: "Files" })).toBeTruthy();
  });
});
