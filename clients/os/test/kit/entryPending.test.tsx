import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EntryPending, useEntryTransition } from "../../src/kit/EntryPending";

function Destination({ pending }: { pending: boolean }) {
  const transition = useEntryTransition(pending);
  return transition.show ? <EntryPending label="Opening your desktop" leaving={transition.leaving} /> : <button>Desktop</button>;
}
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

describe("entry loading transition", () => {
  it("shows one short visible line and waits only for the bounded fade before mounting the destination", async () => {
    vi.useFakeTimers();
    const view = render(<Destination pending />);
    expect(screen.getByText("Opening your desktop").className).not.toContain("os-sr-only");
    expect(screen.queryByRole("button", { name: "Desktop" })).toBeNull();
    view.rerender(<Destination pending={false} />);
    expect(screen.getByRole("status").getAttribute("data-leaving")).toBe("true");
    await act(async () => vi.advanceTimersByTime(159));
    expect(screen.queryByRole("button", { name: "Desktop" })).toBeNull();
    await act(async () => vi.advanceTimersByTime(1));
    expect(screen.getByRole("button", { name: "Desktop" })).toBeTruthy();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("cancels an unfinished exit if entry is still pending and cleans up on unmount", async () => {
    vi.useFakeTimers();
    const view = render(<Destination pending />);
    view.rerender(<Destination pending={false} />);
    await act(async () => vi.advanceTimersByTime(80));
    view.rerender(<Destination pending />);
    await act(async () => vi.advanceTimersByTime(200));
    expect(screen.getByRole("status").hasAttribute("data-leaving")).toBe(false);
    expect(screen.queryByRole("button", { name: "Desktop" })).toBeNull();
    view.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not delay the destination under reduced motion", () => {
    const original = window.matchMedia;
    vi.stubGlobal("matchMedia", (query: string) => query === "(prefers-reduced-motion: reduce)"
      ? { ...original(query), matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() }
      : original(query));
    const view = render(<Destination pending />);
    view.rerender(<Destination pending={false} />);
    expect(screen.getByRole("button", { name: "Desktop" })).toBeTruthy();
    expect(screen.queryByRole("status")).toBeNull();
  });
});
