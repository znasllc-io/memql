import { fireEvent, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { applyRememberedThemePack, rememberThemePack } from "../../src/themes/bootHint";
import { THEME_STYLE_ID } from "../../src/themes/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { gotoSection, memStorage, renderShell } from "../settings/shellHarness";

// THE FIRST FRAME WEARS THE PERSON'S THEME (José, Nov 20 2026 call): the
// loading and sign-in screens start in the last theme this browser showed,
// and in Graphite (light or dark as the computer is set) when there is none.

const KEY = "memql-os-theme-pack";

function freshPage() {
  document.documentElement.removeAttribute("data-os-theme");
  document.getElementById(THEME_STYLE_ID)?.remove();
}

beforeEach(() => {
  resetIdsForTest();
  freshPage();
  localStorage.removeItem(KEY);
});

describe("the remembered theme", () => {
  it("leaves a first visit on Graphite", () => {
    expect(applyRememberedThemePack()).toBe("");
    expect(document.documentElement.hasAttribute("data-os-theme")).toBe(false);
  });

  it("puts a remembered pack and its rules on the page before anything else runs", () => {
    rememberThemePack("vellum", ':root[data-os-theme="vellum"] { --os-ground: #f4f1ea; }');
    freshPage();
    expect(applyRememberedThemePack()).toBe("vellum");
    expect(document.documentElement.getAttribute("data-os-theme")).toBe("vellum");
    expect(document.getElementById(THEME_STYLE_ID)?.textContent).toContain('[data-os-theme="vellum"]');
  });

  it("forgets on Graphite, and ignores a value it cannot read", () => {
    rememberThemePack("vellum", "x");
    rememberThemePack("graphite", "");
    expect(localStorage.getItem(KEY)).toBeNull();
    localStorage.setItem(KEY, "{not json");
    expect(applyRememberedThemePack()).toBe("");
    localStorage.setItem(KEY, JSON.stringify({ id: 7 }));
    expect(applyRememberedThemePack()).toBe("");
  });
});

describe("the desktop remembers what it shows", () => {
  it("remembers a picked theme for the next loading screen, and forgets it on Graphite", async () => {
    renderShell({ storage: memStorage() });
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    gotoSection("Profile", "Appearance");
    await screen.findByRole("radiogroup", { name: "Theme" });

    fireEvent.click(screen.getByRole("radio", { name: "Vellum (built in)" }));
    const remembered = JSON.parse(localStorage.getItem(KEY) ?? "{}");
    expect(remembered.id).toBe("vellum");
    expect(remembered.css).toContain('[data-os-theme="vellum"]');

    // The next boot, before React: the page starts in Vellum.
    freshPage();
    expect(applyRememberedThemePack()).toBe("vellum");
    expect(document.documentElement.getAttribute("data-os-theme")).toBe("vellum");

    fireEvent.click(screen.getByRole("radio", { name: "Graphite (built in)" }));
    expect(localStorage.getItem(KEY)).toBeNull();
  });
});
