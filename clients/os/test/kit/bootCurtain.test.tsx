import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { BootCurtain, bootFinished, bootReached, resetBootCurtainForTests } from "../../src/kit/BootCurtain";

const lit = (root: HTMLElement) => root.querySelectorAll(".os-boot-dot[data-on]").length;
const steps = (root: HTMLElement) => [...root.querySelectorAll(".os-boot-step")].map((p) => `${p.textContent}@${p.getAttribute("data-depth")}`);
// Small steps, so React renders between timers the way a browser does.
const tick = async (ms: number) => { for (let t = 0; t < ms; t += 10) await act(async () => { vi.advanceTimersByTime(Math.min(10, ms - t)); }); if (ms === 0) await act(async () => { vi.advanceTimersByTime(0); }); };

beforeEach(() => { resetBootCurtainForTests(); vi.useFakeTimers(); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

describe("first-load curtain", () => {
  it("lights the mark only as far as the real steps have gone, then blooms away", async () => {
    const { container } = render(<BootCurtain />);
    await tick(1000);
    expect(lit(container)).toBe(1);
    expect(steps(container)).toEqual(["Signing you in@0"]);

    await act(async () => bootReached(1));
    await tick(1000);
    expect(lit(container)).toBe(3);
    expect(steps(container)).toEqual(["Signing you in@1", "Reaching your cluster@0"]);
    // Dots 1 and 3 are lit, but the line between them runs through dark dot 4.
    expect(container.querySelector('.os-boot-edge[x1="13.09"][x2="2.52"]')?.hasAttribute("data-on")).toBe(false);

    await act(async () => bootReached(2));
    await tick(1000);
    expect(lit(container)).toBe(6);
    expect(container.querySelector('.os-boot-edge[x1="13.09"][x2="2.52"]')?.hasAttribute("data-on")).toBe(true);

    await act(async () => bootFinished());
    await tick(3 * 110);
    expect(lit(container)).toBe(9);
    expect(steps(container)).toEqual(["Signing you in@3", "Reaching your cluster@2", "Loading your desktop@1", "Ready@0"]);
    expect(container.querySelector(".os-boot-curtain")?.getAttribute("aria-hidden")).toBe("true");
    await tick(180);
    expect(container.querySelector(".os-boot-curtain")?.hasAttribute("data-leaving")).toBe(true);
    await tick(650);
    expect(container.querySelector(".os-boot-curtain")).toBeNull();
  });

  it("is gone at once under reduced motion, and never shows again in the same page load", async () => {
    const original = window.matchMedia;
    vi.stubGlobal("matchMedia", (query: string) => query === "(prefers-reduced-motion: reduce)"
      ? { ...original(query), matches: true } : original(query));
    const { container } = render(<BootCurtain />);
    await act(async () => bootFinished());
    for (let i = 0; i < 12; i++) await tick(0);
    expect(container.querySelector(".os-boot-curtain")).toBeNull();
    cleanup();
    expect(render(<BootCurtain />).container.querySelector(".os-boot-curtain")).toBeNull();
  });

  it("shows only the steps that really happened on the way to a sign-in screen", async () => {
    const { container } = render(<BootCurtain />);
    await tick(200);
    await act(async () => bootFinished());
    await act(async () => bootReached(1));
    await tick(9 * 110);
    expect(steps(container)).toEqual(["Signing you in@1", "Ready@0"]);
    expect(lit(container)).toBe(9);
  });

  it("opens by itself if no step ever reports, so it cannot hold the OS", async () => {
    const { container } = render(<BootCurtain />);
    await tick(11_999);
    expect(container.querySelector(".os-boot-curtain")).not.toBeNull();
    await tick(1 + 9 * 110 + 180 + 650);
    expect(container.querySelector(".os-boot-curtain")).toBeNull();
  });
});
