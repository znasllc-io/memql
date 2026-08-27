import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import type { ChromeLayout } from "../src/app/layout";

function renderLayout(layout: ChromeLayout) {
  render(<Shell layout={layout} onSignOut={vi.fn()} />);
}

describe("OS chrome", () => {
  it.each(["phone", "ipad", "desktop"] as const)("keeps sign out on the %s layout", (layout) => {
    renderLayout(layout);
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
  });

  it("reserves two empty slots only on the desktop pointer frame", () => {
    renderLayout("desktop");
    expect(document.querySelectorAll("[data-slot]")).toHaveLength(2);
    expect(document.querySelector("[data-slot='a']")?.textContent).toBe("");
    expect(document.querySelector("[data-slot='b']")?.textContent).toBe("");
  });

  it("does not paint reserved slots on phone chrome", () => {
    renderLayout("phone");
    expect(document.querySelector("[data-slots]")).toBeNull();
  });

  it("does not paint reserved slots on iPad chrome", () => {
    renderLayout("ipad");
    expect(document.querySelector("[data-slots]")).toBeNull();
  });

  it("exposes a mode switcher with light, dark, and system", () => {
    renderLayout("phone");
    expect(screen.getByRole("button", { name: "Light" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dark" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "System" })).toBeTruthy();
  });
});
