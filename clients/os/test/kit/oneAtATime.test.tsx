import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Button } from "../../src/kit/controls";
import { IconButton } from "../../src/kit/IconButton";
import { DONE_MS, MIN_HOLD_MS } from "../../src/kit/oneAtATime";

// ONE PRESS AT A TIME (kit/oneAtATime; José, Nov 19: twenty clicks on Save
// made eighteen copies). A button whose act returns a promise takes no second
// press until it settles; a labelled one says "Saving…", then "Saved ✓" for
// two seconds; an icon only dims (Jesús, Nov 20). Either way the button
// holds for at least half a second, however fast the act finishes (Jesús,
// Nov 20 2026: spamming Download filled his Downloads folder, because a
// local download finishes in a few hundredths of a second).

function deferred<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => { vi.useRealTimers(); });

describe("a labelled button", () => {
  it("counts a burst as one press, says its busy word, then its done word with a tick for two seconds, then is itself again", async () => {
    const save = deferred();
    const act1 = vi.fn(() => save.promise);
    render(<Button onClick={act1} busyLabel="Saving…" doneLabel="Saved">Save to my Library</Button>);
    const button = screen.getByRole("button");

    act(() => { for (let i = 0; i < 20; i++) fireEvent.click(button); });
    expect(act1).toHaveBeenCalledOnce();
    expect(button.textContent).toBe("Saving…");
    expect(button.getAttribute("aria-busy")).toBe("true");

    await act(async () => { save.resolve(); });
    act(() => { vi.advanceTimersByTime(MIN_HOLD_MS); });
    expect(button.textContent).toBe("Saved");
    expect(button.querySelector("svg")).not.toBeNull();
    expect(button.getAttribute("data-state")).toBe("done");
    // The done word takes no press either.
    act(() => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledOnce();

    act(() => { vi.advanceTimersByTime(DONE_MS); });
    expect(button.textContent).toBe("Save to my Library");
    expect(button.hasAttribute("data-state")).toBe(false);
    act(() => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledTimes(2);
  });

  it("never says done for an act that answered false, and is pressable again after the half second", async () => {
    const act1 = vi.fn(async () => false);
    render(<Button onClick={act1} busyLabel="Saving…" doneLabel="Saved">Save</Button>);
    const button = screen.getByRole("button");
    await act(async () => { fireEvent.click(button); });
    act(() => { vi.advanceTimersByTime(MIN_HOLD_MS); });
    expect(button.textContent).toBe("Save");
    await act(async () => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledTimes(2);
  });

  it("says its busy word for at least half a second, even when the act finishes at once", async () => {
    const act1 = vi.fn(async () => undefined);
    render(<Button onClick={act1} busyLabel="Saving…" doneLabel="Saved">Save</Button>);
    const button = screen.getByRole("button");
    await act(async () => { fireEvent.click(button); });
    expect(button.textContent).toBe("Saving…");
    await act(async () => { vi.advanceTimersByTime(MIN_HOLD_MS - 1); fireEvent.click(button); });
    expect(act1).toHaveBeenCalledOnce();
    expect(button.textContent).toBe("Saving…");
    act(() => { vi.advanceTimersByTime(1); });
    expect(button.textContent).toBe("Saved");
  });

  it("without a done word goes straight back when the act finishes", async () => {
    const run = deferred();
    render(<Button onClick={() => run.promise} busyLabel="Deleting…">Delete</Button>);
    const button = screen.getByRole("button");
    act(() => { fireEvent.click(button); });
    expect(button.textContent).toBe("Deleting…");
    await act(async () => { run.resolve(); });
    act(() => { vi.advanceTimersByTime(MIN_HOLD_MS); });
    expect(button.textContent).toBe("Delete");
  });

  it("holds nothing for an act that returns no promise", () => {
    const open = vi.fn();
    render(<Button onClick={open}>Open</Button>);
    const button = screen.getByRole("button");
    act(() => { fireEvent.click(button); fireEvent.click(button); });
    expect(open).toHaveBeenCalledTimes(2);
  });
});

describe("an icon-only button", () => {
  it("dims while its act runs, takes no second press, and has no done word", async () => {
    const save = deferred();
    const act1 = vi.fn(() => save.promise);
    render(<IconButton label="Save to my Library" onClick={act1}><svg /></IconButton>);
    const button = screen.getByRole("button", { name: "Save to my Library" });
    act(() => { for (let i = 0; i < 20; i++) fireEvent.click(button); });
    expect(act1).toHaveBeenCalledOnce();
    expect(button.hasAttribute("data-busy")).toBe(true);
    // Not `disabled`: it keeps keyboard focus while it works.
    expect((button as HTMLButtonElement).disabled).toBe(false);

    await act(async () => { save.resolve(); });
    act(() => { vi.advanceTimersByTime(MIN_HOLD_MS); });
    expect(button.hasAttribute("data-busy")).toBe(false);
    act(() => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledTimes(2);
  });

  it("stays dim and takes no press for half a second, even when its act finishes at once (a local download)", async () => {
    const download = vi.fn(async () => undefined);
    render(<IconButton label="Download" onClick={download}><svg /></IconButton>);
    const button = screen.getByRole("button", { name: "Download" });
    // Spam: a click every 50 ms for just under half a second.
    for (let t = 0; t < MIN_HOLD_MS; t += 50) {
      await act(async () => { fireEvent.click(button); vi.advanceTimersByTime(50 - 1); });
      expect(button.hasAttribute("data-busy")).toBe(true);
      await act(async () => { vi.advanceTimersByTime(1); });
    }
    expect(download).toHaveBeenCalledOnce();
    expect(button.hasAttribute("data-busy")).toBe(false);
    await act(async () => { fireEvent.click(button); });
    expect(download).toHaveBeenCalledTimes(2);
  });
});
