import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks } from "../../src/ask/askController";
afterEach(cleanup);

it.each([
  ['automation "workSpineDraft" execution failed: Spine refused: work compile: triage fileFormat: compose: unknown format "png"', false],
  ['every_door_shut: local image model unavailable', true],
  ['A detailed storage failure\nwith diagnostic context', false],
] as const)("offers recovery for the actual failure: %s", (message, fleet) => {
  let callbacks: AskCallbacks;
  const ask = vi.fn((_prompt: string, _context: string | null, on: AskCallbacks) => { callbacks = on; return { cancel: vi.fn() }; });
  const openFleet = vi.fn();
  render(<AskSurface transport={{ ask }} variant="sheet" availability={{ state:"ready",message:"",refresh:vi.fn() }} onOpenFleet={openFleet} />);
  fireEvent.change(screen.getByRole("textbox", {name:"Ask"}), {target:{value:"Create my requested artifact"}});
  fireEvent.click(screen.getByRole("button", {name:"Send"}));
  act(() => callbacks.error(message));
  expect(screen.getByRole("alert").textContent).not.toBe(message);
  expect(screen.getByText("Details").closest("details")?.textContent).toContain(message);
  const button = screen.queryByRole("button", {name:"Open Fleet"});
  expect(Boolean(button)).toBe(fleet);
  if (button) { fireEvent.click(button); expect(openFleet).toHaveBeenCalledOnce(); }
  fireEvent.click(screen.getByRole("button", {name:"Edit and try again"}));
  expect(ask).toHaveBeenCalledOnce();
  expect(screen.getByRole("textbox", {name:"Ask"})).toHaveProperty("value", "Create my requested artifact");
});
