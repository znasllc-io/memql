import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { COBALT, VELLUM } from "../../src/themes/builtins";
import { THEME_STYLE_ID } from "../../src/themes/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { DESKTOP_STORE_KEY } from "../../src/system/store";
import { memStorage, renderShell } from "../settings/shellHarness";

// The marketplace, against the REAL shell -- because the thing being tested
// is what happens to the DESKTOP, and a mounted-in-isolation drawer would
// prove only that a component called a function.

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-os-theme");
  document.getElementById(THEME_STYLE_ID)?.remove();
});

function openThemes() {
  fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
  fireEvent.click(
    within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", {
      name: /Themes/,
    }),
  );
  return screen.getByRole("dialog", { name: "Themes" });
}

const wearing = () => document.documentElement.getAttribute("data-os-theme");

/** A pack file, as a person would be handed one. */
function packFile(overrides: Record<string, unknown> = {}): File {
  const body = JSON.stringify({ ...JSON.parse(JSON.stringify(VELLUM)), id: "sold", builtIn: undefined, ...overrides });
  return new File([body], "sold.json", { type: "application/json" });
}

describe("the theme marketplace", () => {
  it("opens from the Launcher, and closes it on the way", () => {
    renderShell();
    const drawer = openThemes();
    expect(drawer).toBeTruthy();
    // THE LAUNCHER MUST GO. It is a full-screen glass overlay, and the whole
    // gesture here is watching the desktop restyle behind the drawer.
    expect(screen.queryByRole("dialog", { name: "Launcher" })).toBeNull();
  });

  it("wears a theme while you point at it, and takes it off when you leave", () => {
    renderShell();
    const drawer = openThemes();
    expect(wearing()).toBe("graphite");

    const cobalt = within(drawer).getByRole("button", { name: `Use ${COBALT.name}` });
    fireEvent.pointerEnter(cobalt);
    expect(wearing()).toBe("cobalt");

    fireEvent.pointerLeave(cobalt);
    expect(wearing()).toBe("graphite");
  });

  it("previews on FOCUS too", () => {
    // Not a nicety: the preview is the product, and a keyboard reader who
    // could only preview by choosing would be shopping by trying things on
    // permanently.
    renderShell();
    const drawer = openThemes();
    const vellum = within(drawer).getByRole("button", { name: `Use ${VELLUM.name}` });
    fireEvent.focus(vellum);
    expect(wearing()).toBe("vellum");
    fireEvent.blur(vellum);
    expect(wearing()).toBe("graphite");
  });

  it("a preview is never stored, and a choice always is", () => {
    const storage = memStorage();
    renderShell({ storage });
    const drawer = openThemes();
    const cobalt = within(drawer).getByRole("button", { name: `Use ${COBALT.name}` });

    fireEvent.pointerEnter(cobalt);
    // Hovering must not roam somebody's other machine to a theme they were
    // only looking at.
    expect(JSON.parse(storage.getItem(DESKTOP_STORE_KEY)!).themePack).toBe("graphite");

    fireEvent.click(cobalt);
    expect(wearing()).toBe("cobalt");
    expect(JSON.parse(storage.getItem(DESKTOP_STORE_KEY)!).themePack).toBe("cobalt");
  });

  it("closing mid-hover puts the desktop back", () => {
    renderShell();
    const drawer = openThemes();
    fireEvent.pointerEnter(within(drawer).getByRole("button", { name: `Use ${VELLUM.name}` }));
    expect(wearing()).toBe("vellum");

    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Themes" })).toBeNull();
    // Otherwise the desktop keeps wearing the last card the pointer crossed
    // on its way out, which reads as a theme that applied itself.
    expect(wearing()).toBe("graphite");
  });

  it("installs a pack from a file, wears it, and keeps it", async () => {
    const storage = memStorage();
    renderShell({ storage });
    const drawer = openThemes();
    const input = drawer.querySelector('input[type="file"]') as HTMLInputElement;

    fireEvent.change(input, { target: { files: [packFile()] } });
    await waitFor(() => expect(wearing()).toBe("sold"));

    const doc = JSON.parse(storage.getItem(DESKTOP_STORE_KEY)!);
    expect(doc.themePack).toBe("sold");
    expect(doc.installedPacks.map((p: { id: string }) => p.id)).toEqual(["sold"]);
    // Built-ins are never stored -- the bundle already has them.
    expect(doc.installedPacks).toHaveLength(1);

    // Its CSS is in the document, so the tokens actually resolve.
    const style = document.getElementById(THEME_STYLE_ID);
    expect(style?.textContent).toContain(':root[data-os-theme="sold"]');
  });

  it("refuses a broken pack by naming what is wrong with it", async () => {
    renderShell();
    const drawer = openThemes();
    const input = drawer.querySelector('input[type="file"]') as HTMLInputElement;

    const broken = JSON.parse(JSON.stringify(VELLUM));
    broken.id = "broken";
    delete broken.builtIn;
    delete broken.tokens.light.accent;

    fireEvent.change(input, {
      target: { files: [new File([JSON.stringify(broken)], "broken.json")] },
    });

    expect(await screen.findByText(/missing 1 light colour: accent/i)).toBeTruthy();
    expect(wearing()).toBe("graphite");
  });

  it("refuses a pack claiming a built-in id", async () => {
    renderShell();
    const drawer = openThemes();
    const input = drawer.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [packFile({ id: "graphite" })] } });
    expect(await screen.findByText(/is a built-in theme/i)).toBeTruthy();
  });

  it("uninstalling the pack you are wearing lands on graphite", async () => {
    renderShell();
    const drawer = openThemes();
    const input = drawer.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [packFile()] } });
    await waitFor(() => expect(wearing()).toBe("sold"));

    fireEvent.click(await screen.findByRole("button", { name: /Remove Vellum/ }));
    // Graphite is the only answer that always exists: its tokens are the
    // bundle's unqualified :root.
    expect(wearing()).toBe("graphite");
  });

  it("offers no Remove on a built-in", () => {
    renderShell();
    const drawer = openThemes();
    expect(within(drawer).queryByRole("button", { name: `Remove ${VELLUM.name}` })).toBeNull();
  });
});
