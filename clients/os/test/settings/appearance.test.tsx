import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { BUILT_IN_PACKS, resolveThemePack, themePackById } from "../../src/themes/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { DESKTOP_STORE_KEY } from "../../src/system/store";
import { gotoSection, memStorage, openFromLauncher, renderShell } from "./shellHarness";

// The theme-pack selector (memql#4743) -- the ONE write this epic makes,
// and it goes to DesktopStore, never to the cluster.

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-os-theme");
});

describe("the theme registry", () => {
  it("ships three built-ins, graphite first", () => {
    // Graphite is FIRST because it is the fallback: its tokens are the
    // bundle's unqualified :root block, so it is the one pack that is
    // guaranteed to render on the first frame, offline, before any of the
    // theme module has run.
    expect(BUILT_IN_PACKS.map((t) => t.id)).toEqual(["graphite", "vellum", "cobalt"]);
    expect(BUILT_IN_PACKS.every((p) => p.builtIn)).toBe(true);
    expect(themePackById("graphite")?.name).toBe("Graphite");
  });

  it("falls back to the built-in for a pack that is no longer installed", () => {
    // A stored pack can outlive its installation. Leaving the picker with no
    // selection is the alternative, and the desktop document is not where a
    // person should discover an uninstall.
    expect(resolveThemePack("a-pack-from-the-marketplace").id).toBe("graphite");
    expect(resolveThemePack("graphite").id).toBe("graphite");
  });
});

describe("the Appearance section", () => {
  function openAppearance(storage = memStorage()) {
    renderShell({ storage });
    openFromLauncher("Settings");
    gotoSection("Settings", "Appearance");
    return storage;
  }

  it("offers the built-in pack, selected", () => {
    openAppearance();
    const group = screen.getByRole("radiogroup", { name: "Theme" });
    const graphite = within(group).getByRole("radio", { name: "Graphite (built in)" });
    expect(graphite.getAttribute("aria-checked")).toBe("true");
  });

  it("sends you to the drawer to try one on, rather than previewing here", () => {
    // This panel is a WINDOW on the desktop it would be previewing, so it
    // cannot show a preview without previewing itself. An absent affordance
    // with no account of itself reads as an oversight, so it says where the
    // preview lives.
    openAppearance();
    expect(screen.getByText(/Themes in the Launcher/i)).toBeTruthy();
  });

  it("lists every built-in pack", () => {
    openAppearance();
    const group = screen.getByRole("radiogroup", { name: "Theme" });
    for (const name of ["Graphite (built in)", "Vellum (built in)", "Cobalt (built in)"]) {
      expect(within(group).getByRole("radio", { name })).toBeTruthy();
    }
  });

  it("applies the pack as data-os-theme on the document root", () => {
    openAppearance();
    fireEvent.click(screen.getByRole("radio", { name: "Graphite (built in)" }));
    expect(document.documentElement.getAttribute("data-os-theme")).toBe("graphite");
  });

  it("persists the pack through DesktopStore", () => {
    const storage = openAppearance();
    fireEvent.click(screen.getByRole("radio", { name: "Graphite (built in)" }));
    const stored = storage.getItem(DESKTOP_STORE_KEY);
    expect(stored).toBeTruthy();
    expect(JSON.parse(stored!).themePack).toBe("graphite");
  });

  it("never touches mode -- the two controls are orthogonal", () => {
    openAppearance();
    fireEvent.click(screen.getByRole("radio", { name: "Dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");

    fireEvent.click(screen.getByRole("radio", { name: "Graphite (built in)" }));
    // Choosing a theme pack leaves mode exactly where it was. A theme
    // defines BOTH looks; picking one is not a statement about which look.
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(screen.getByRole("radio", { name: "Dark" }).getAttribute("aria-checked")).toBe("true");
  });
});
