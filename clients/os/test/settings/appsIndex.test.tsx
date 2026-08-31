import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { resetIdsForTest } from "../../src/system/desks";
import { ADMIN, READER, gotoSection, openFromLauncher, renderShell } from "./shellHarness";

// The apps index (memql#4743), through the real shell.

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
});

function openIndex(access?: Parameters<typeof renderShell>[0] extends undefined ? never : { access: typeof ADMIN }) {
  renderShell(access);
  openFromLauncher("Settings");
  gotoSection("Settings", "Apps");
  return screen.getByRole("list", { name: "Installed apps" });
}

/** The index row for an app, scoped to the list -- an app's name also
 *  appears on its dock button and its window title. */
function entryFor(list: HTMLElement, name: string): HTMLElement {
  const entry = within(list)
    .getAllByRole("listitem")
    .find((li) => within(li).queryByText(name) !== null);
  if (!entry) throw new Error(`no index entry for ${name}`);
  return entry;
}

describe("the apps index", () => {
  it("lists exactly the apps this session may open", () => {
    const list = openIndex({ access: ADMIN });
    expect(within(list).getByText("Artifacts")).toBeTruthy();
    expect(within(list).getByText("Users")).toBeTruthy();
    // Scoped to the entry: "Settings" is also the label on every app's own
    // settings button in this list.
    expect(entryFor(list, "Settings")).toBeTruthy();
  });

  it("omits admin-gated apps from a reader's index", () => {
    const list = openIndex({ access: READER });
    expect(within(list).queryByText("Users")).toBeNull();
    expect(within(list).queryByText("Training")).toBeNull();
    expect(within(list).getByText("Artifacts")).toBeTruthy();
  });

  it("opens the target app's own window on its settings section", () => {
    const list = openIndex({ access: ADMIN });
    fireEvent.click(within(entryFor(list, "Fleet")).getByRole("button", { name: "Settings" }));

    // The Fleet window is open AND showing its own settings section -- which
    // is not the section the shell opens an app on by default (that is
    // Machines, sections[0]).
    const nav = screen.getByRole("navigation", { name: "Fleet sections" });
    expect(within(nav).getByRole("button", { name: "Settings" }).getAttribute("aria-current")).toBe(
      "page",
    );
  });

  it("focuses the app's EXISTING window rather than opening a second one", () => {
    renderShell({ access: ADMIN });
    openFromLauncher("Fleet");
    openFromLauncher("Settings");
    gotoSection("Settings", "Apps");

    const list = screen.getByRole("list", { name: "Installed apps" });
    fireEvent.click(within(entryFor(list, "Fleet")).getByRole("button", { name: "Settings" }));

    // One Fleet window, on its settings section. Open-by-id is the path
    // that guarantees this; opening a fresh window would leave two.
    expect(screen.getAllByRole("navigation", { name: "Fleet sections" })).toHaveLength(1);
  });

  it("is a directory, not a host -- it never renders another app's settings UI", () => {
    const list = openIndex({ access: ADMIN });
    // The Fleet's own settings surface offers an "open Fleet on" picker.
    // Nothing of it may appear inside the index.
    expect(within(list).queryByRole("combobox")).toBeNull();
    expect(within(list).queryByRole("checkbox")).toBeNull();
  });
});
