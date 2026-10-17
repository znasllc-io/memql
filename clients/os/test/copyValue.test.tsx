import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { CopyValue, Fact, Facts } from "../src/kit";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.getSelection()?.removeAllRanges(); });

function clipboard(writeText = vi.fn().mockResolvedValue(undefined)) {
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  return writeText;
}

it.each(["icon", "row"])("copies the exact timestamp once from the %s and confirms", async target => {
  const write = clipboard();
  render(<Facts><Fact label="Last heartbeat" value="just now" copyValue="2026-10-14T21:00:00Z" /></Facts>);
  const button = screen.getByRole("button", { name: "Copy Last heartbeat" });
  fireEvent.click(screen.getByText("just now"));
  expect(write).not.toHaveBeenCalled();
  fireEvent.click(target === "icon" ? button.querySelector("svg")! : button);
  expect(write).toHaveBeenCalledExactlyOnceWith("2026-10-14T21:00:00Z");
  expect(await screen.findByRole("button", { name: "Copied Last heartbeat" })).toBeTruthy();
  expect(screen.getByRole("status").textContent).toBe("Copied Last heartbeat");
  expect(screen.getByText("just now")).toBeTruthy();
});

it("does not copy while selecting part of a value by hand", () => {
  const write = clipboard();
  render(<CopyValue label="Hostname" value="studio.local" presentation="row" />);
  const value = screen.getByText("studio.local");
  const range = document.createRange();
  range.selectNodeContents(value);
  window.getSelection()?.addRange(range);
  fireEvent.click(value, { detail: 1 });
  expect(write).not.toHaveBeenCalled();
});

it.each(["refused", "unavailable"])("keeps the value available and permits retry when clipboard is %s", async failure => {
  if (failure === "refused") clipboard(vi.fn().mockRejectedValue(new Error("denied")));
  else vi.stubGlobal("navigator", {});
  render(<CopyValue label="Hostname" value="studio.local" presentation="row" />);
  fireEvent.click(screen.getByRole("button", { name: "Copy Hostname" }));
  await screen.findByText(/nothing was copied/);
  expect(screen.getByText("studio.local")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Copied Hostname" })).toBeNull();
  const write = clipboard();
  fireEvent.click(screen.getByRole("button", { name: "Copy Hostname" }));
  await screen.findByRole("button", { name: "Copied Hostname" });
  expect(write).toHaveBeenCalledExactlyOnceWith("studio.local");
});

it("ignores a copy finishing after the live value changes", async () => {
  let finish!: () => void;
  clipboard(vi.fn().mockReturnValue(new Promise<void>(resolve => { finish = resolve; })));
  const view = render(<CopyValue label="Hostname" value="old.local" presentation="row" />);
  fireEvent.click(screen.getByRole("button", { name: "Copy Hostname" }));
  view.rerender(<CopyValue label="Hostname" value="new.local" presentation="row" />);
  await act(async () => finish());
  expect(screen.queryByRole("button", { name: "Copied Hostname" })).toBeNull();
  expect(screen.getByText("new.local")).toBeTruthy();
});

it("offers no copy action for an unreported fact", () => {
  render(<Facts><Fact label="Build" value="" copyValue="" /></Facts>);
  expect(screen.queryByRole("button")).toBeNull();
  expect(screen.getByText("--")).toBeTruthy();
});

it("preserves the compact copy control for existing callers", async () => {
  const write = clipboard();
  render(<CopyValue label="Id" value="record-1" />);
  fireEvent.click(screen.getByText("record-1"));
  expect(write).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Copy Id" }));
  await screen.findByRole("button", { name: "Copied" });
  expect(write).toHaveBeenCalledExactlyOnceWith("record-1");
});
