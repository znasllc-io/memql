import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Button } from "../../src/kit/controls";
import { IconButton } from "../../src/kit/IconButton";
import { DONE_MS } from "../../src/kit/oneAtATime";

// ONE PRESS AT A TIME (kit/oneAtATime; José, Nov 19: twenty clicks on Save
// made eighteen copies). A button whose act returns a promise takes no second
// press until it settles; a labelled one says "Saving…", then "Saved ✓" for
// two seconds; an icon only dims (Jesús, Nov 20).

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

  it("never says done for an act that answered false, and is pressable again at once", async () => {
    const act1 = vi.fn(async () => false);
    render(<Button onClick={act1} busyLabel="Saving…" doneLabel="Saved">Save</Button>);
    const button = screen.getByRole("button");
    await act(async () => { fireEvent.click(button); });
    expect(button.textContent).toBe("Save");
    await act(async () => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledTimes(2);
  });

  it("without a done word goes straight back when the act finishes", async () => {
    const run = deferred();
    render(<Button onClick={() => run.promise} busyLabel="Deleting…">Delete</Button>);
    const button = screen.getByRole("button");
    act(() => { fireEvent.click(button); });
    expect(button.textContent).toBe("Deleting…");
    await act(async () => { run.resolve(); });
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
    expect(button.hasAttribute("data-busy")).toBe(false);
    act(() => { fireEvent.click(button); });
    expect(act1).toHaveBeenCalledTimes(2);
  });
});
