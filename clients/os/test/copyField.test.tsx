import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CopyField } from "../src/kit";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });

function clipboard(writeText = vi.fn().mockResolvedValue(undefined)) {
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  return writeText;
}

describe("copying a whole command field", () => {
  it.each(["text", "padding", "button", "icon"])("copies exactly once from %s and visibly confirms", async (target) => {
    const write = clipboard();
    const { container } = render(<CopyField value="memql worker setup" label="the command" />);
    const field = screen.getByRole("textbox", { name: "the command" });
    const button = screen.getByRole("button", { name: "Copy the command" });
    const hit = target === "text" ? field : target === "button" ? button : target === "icon" ? button.querySelector("svg")! : container.querySelector(".os-copyfield-line")!;
    fireEvent.click(hit);
    // Called synchronously in the trusted gesture, before any await.
    expect(write).toHaveBeenCalledExactlyOnceWith("memql worker setup");
    expect((await screen.findByRole("button", { name: "Copied" })).textContent).toBe("Copied");
    expect(screen.getByRole("status").textContent).toBe("Copied the command");
  });

  it.each(["Enter", " "])("copies with %s from the selectable field", async (key) => {
    const write = clipboard();
    render(<CopyField value="command" label="the command" />);
    fireEvent.keyDown(screen.getByRole("textbox"), { key });
    await screen.findByRole("button", { name: "Copied" });
    expect(write).toHaveBeenCalledExactlyOnceWith("command");
  });

  it("copies on repeated clicks and keeps the receipt for two seconds after the latest one", async () => {
    vi.useFakeTimers();
    const write = clipboard();
    render(<CopyField value="command" label="the command" />);
    await act(async () => { fireEvent.click(screen.getByRole("textbox")); });
    act(() => vi.advanceTimersByTime(1500));
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Copied" })); });
    act(() => vi.advanceTimersByTime(1500));
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
    act(() => vi.advanceTimersByTime(500));
    expect(screen.getByRole("button", { name: "Copy the command" })).toBeTruthy();
    expect(write).toHaveBeenCalledTimes(2);
  });

  it.each(["refused", "unavailable"])("keeps manual selection available when the clipboard is %s", async (failure) => {
    if (failure === "refused") clipboard(vi.fn().mockRejectedValue(new Error("denied")));
    else vi.stubGlobal("navigator", {});
    render(<CopyField value="command" label="the command" />);
    fireEvent.click(screen.getByRole("textbox"));
    await screen.findByText(/select the text and copy it/);
    expect(screen.queryByRole("button", { name: "Copied" })).toBeNull();
    const input = screen.getByRole("textbox") as HTMLInputElement;
    input.focus();
    input.select();
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(input.value.length);
    expect(input.readOnly).toBe(true);
  });

  it("does not confirm a newer command when an older copy finishes", async () => {
    let resolve!: () => void;
    clipboard(vi.fn().mockReturnValue(new Promise<void>(done => { resolve = done; })));
    const view = render(<CopyField value="old" label="the command" />);
    fireEvent.click(screen.getByRole("textbox"));
    view.rerender(<CopyField value="new" label="the command" />);
    await act(async () => resolve());
    expect(screen.queryByRole("button", { name: "Copied" })).toBeNull();
  });
});
