import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";
afterEach(cleanup);
const ready = { state: "ready" as const, message: "", refresh: vi.fn() };
function wire() {
  let callbacks: AskCallbacks | null = null;
  const cancel = vi.fn();
  const ask = vi.fn((_p: string, _c: string | null, on: AskCallbacks) => { callbacks = on; return { cancel }; });
  return { transport: { ask }, ask, cancel, callbacks: () => callbacks! };
}
function typePrompt(text = "Can my machine answer?") { fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: text } }); }
function send() { fireEvent.click(screen.getByRole("button", { name: "Send" })); }
it("keeps draft while unavailable and only disables Send (no Open Fleet banner)", () => {
  const w = wire(); const open = vi.fn();
  const availability = { ...ready, state: "unavailable" as const, message: "" };
  const props = { transport: w.transport, variant: "sheet" as const, availability, onOpenFleet: open };
  const view = render(<AskSurface {...props} />); typePrompt();
  const button = screen.getByRole("button", { name: "Send" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Open Fleet" })).toBeNull();
  expect(screen.queryByText(/No chat model is available/)).toBeNull();
  fireEvent.submit(button.closest("form")!); expect(w.ask).not.toHaveBeenCalled();
  view.rerender(<AskSurface {...props} availability={ready} />);
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement).value).toBe("Can my machine answer?");
  send(); expect(w.ask).toHaveBeenCalledOnce();
});
it("blocks loading before readiness is known", () => {
  const w = wire(); render(<AskSurface {...{ transport: w.transport, variant: "widget" as const, availability: { ...ready, state: "checking" as const, message: "" } }} />);
  typePrompt(); send(); expect(w.ask).not.toHaveBeenCalled();
  expect(screen.queryByText(/Checking whether chat/)).toBeNull();
});
it("shows waiting and Stop, preserves partial text, and retries a stopped reply", () => {
  const w = wire(); render(<AskSurface {...{ transport: w.transport, variant: "sheet" as const, availability: ready }} />);
  typePrompt(); send(); expect(screen.getByText(/Waiting for an answer/)).toBeTruthy();
  act(() => w.callbacks().delta("First part"));
  fireEvent.click(screen.getByRole("button", { name: "Stop reply" }));
  expect(w.cancel).toHaveBeenCalledOnce(); expect(screen.getByRole("alert").textContent).toMatch(/Stopped/);
  expect(screen.getByText("First part")).toBeTruthy();
  act(() => { w.callbacks().delta("late text"); w.callbacks().done(); });
  expect(screen.queryByText(/late text/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Retry" })); expect(w.ask).toHaveBeenCalledTimes(2);
});
it("ends waiting visibly on disconnect and preserves a new draft", () => {
  const w = wire(); const props = { transport: w.transport, variant: "sheet" as const, availability: ready };
  const view = render(<AskSurface {...props} />); typePrompt(); send(); typePrompt("next question");
  view.rerender(<AskSurface {...props} availability={{ ...ready, state: "disconnected", message: "Connection to the cluster was lost." }} />);
  expect(screen.getByRole("alert").textContent).toMatch(/connection|Connection/);
  expect(screen.queryByRole("button", { name: "Stop reply" })).toBeNull(); expect(w.cancel).toHaveBeenCalledOnce();
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement).value).toBe("next question");
});
it("catches synchronous transport failure", () => {
  const transport: AskTransport = { ask: () => { throw new Error("The worker disconnected."); } };
  render(<AskSurface {...{ transport, variant: "sheet" as const, availability: ready }} />); typePrompt(); send();
  expect(screen.getByRole("alert").textContent).toMatch(/worker disconnected/);
  expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
});
it("rejects empty completion and ignores callbacks after termination", () => {
  const w = wire(); render(<AskSurface {...{ transport: w.transport, variant: "sheet" as const, availability: ready }} />); typePrompt(); send();
  act(() => w.callbacks().done()); expect(screen.getByRole("alert").textContent).toMatch(/without an answer/);
  act(() => w.callbacks().delta("late reply")); expect(screen.queryByText("late reply")).toBeNull();
});
it("defaults to checking when a caller has no authoritative readiness", () => {
  const w = wire(); render(<AskSurface transport={w.transport} variant="sheet" />); typePrompt(); send();
  expect(w.ask).not.toHaveBeenCalled();
  expect(screen.queryByText(/Checking whether chat/)).toBeNull();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
});
it("keeps a long diagnostic in details and leaves a readable failure and recovery actions", () => {
  const w = wire(); const diagnostic = "every_door_shut " + "internal-provider-decision ".repeat(30);
  render(<AskSurface transport={w.transport} variant="sheet" availability={ready} onOpenFleet={vi.fn()} />);
  typePrompt(); send(); act(() => w.callbacks().error(diagnostic));
  expect(screen.getByRole("alert").textContent).toBe("The cluster could not complete this reply. Retry, or check the available models in Fleet.");
  expect(screen.getByText("Error details").closest("details")?.textContent).toContain(diagnostic);
  expect(screen.getByRole("button", { name: "Open Fleet" })).toBeTruthy();
});
