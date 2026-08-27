import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import type { ChromeLayout } from "../src/app/layout";

function renderLayout(layout: ChromeLayout) {
  render(<Shell layout={layout} onSignOut={vi.fn()} />);
}

describe("OS chrome", () => {
  it.each(["phone", "ipad", "desktop"] as const)("keeps sign out on the %s layout", (layout) => {
    renderLayout(layout);
    const button = screen.getByRole("button", { name: "Sign out" });
    expect(button).toBeTruthy();
    expect(button.closest(".os-chrome-actions")).toBeTruthy();
  });

  it("paints two slot roots on the desktop pointer frame", () => {
    renderLayout("desktop");
    const slots = document.querySelectorAll("[data-os-slot]");
    expect(slots).toHaveLength(2);
    expect(document.querySelector("[data-os-slot='a']")).toBeTruthy();
    expect(document.querySelector("[data-os-slot='b']")).toBeTruthy();
  });

  it("paints one or two slot roots on iPad, never hover-only chrome", () => {
    renderLayout("ipad");
    const slots = document.querySelectorAll("[data-os-slot]");
    expect(slots.length).toBeGreaterThanOrEqual(1);
    expect(slots.length).toBeLessThanOrEqual(2);
    expect(screen.getByRole("button", { name: "Profile" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Coming soon" })).toBeTruthy();
  });

  it("does not paint slots on phone chrome", () => {
    renderLayout("phone");
    expect(document.querySelector("[data-os-slot]")).toBeNull();
    expect(document.querySelector("[data-os-module='profile']")).toBeTruthy();
    expect(document.querySelector("[data-os-research]")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Coming soon" })).toBeNull();
  });

  it("puts the theme token pack on each slot root", () => {
    renderLayout("desktop");
    for (const slot of document.querySelectorAll("[data-os-slot]")) {
      expect(slot.hasAttribute("data-os-tokens")).toBe(true);
    }
  });

  it("shows a coming-soon tile on desktop that does not open a store", () => {
    renderLayout("desktop");
    const tile = screen.getByRole("button", { name: "Coming soon" });
    expect(tile).toBeTruthy();
    fireEvent.click(tile);
    expect(document.querySelector("[data-os-store]")).toBeNull();
    expect(document.querySelector("[data-os-catalog]")).toBeNull();
    expect(screen.queryByText(/theme store/i)).toBeNull();
  });

  it("exposes a mode switcher with light, dark, and system", () => {
    renderLayout("phone");
    expect(screen.getByRole("button", { name: "Light" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dark" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "System" })).toBeTruthy();
  });
});
