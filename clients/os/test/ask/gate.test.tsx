import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { coreAt, withOs } from "../setup/harness";
import { withSession } from "../cluster/harness";
import { AskProvider, useAsk } from "../../src/ask/AskProvider";
import { AskSheet } from "../../src/ask/AskSheet";
import { OS_REGISTRY } from "../../src/apps/registry";
import { SETUP_WIDGET_SIZE } from "../../src/apps/setup/manifest";
import { READY_ASK, type AskAvailability } from "../../src/ask/useAskReadiness";

afterEach(cleanup);
function OpenOnMount() { const { openAsk } = useAsk(); useEffect(() => openAsk(), [openAsk]); return null; }
const Widget = OS_REGISTRY.widgets.find((w) => w.id === "ask")!.component;

it("Ask widget matches Set up desk footprint", () => {
  const ask = OS_REGISTRY.widgets.find((w) => w.id === "ask")!;
  const setup = OS_REGISTRY.widgets.find((w) => w.id === "setup")!;
  expect(ask.size).toEqual(SETUP_WIDGET_SIZE);
  expect(setup.size).toEqual(SETUP_WIDGET_SIZE);
  expect(ask.size).toEqual(setup.size);
});

it.each(["sheet", "widget"])("keeps %s input visible but blocks Send until the same authoritative reading permits it", (entry) => {
  const ask = vi.fn(() => ({ cancel: vi.fn() }));
  function tree(availability?: AskAvailability) {
    return withSession(withOs(
      <AskProvider transport={{ ask }} availability={availability}>
        {entry === "sheet" ? <><OpenOnMount /><AskSheet /></> : <Widget />}
      </AskProvider>, "owner"), { readiness: coreAt("unconfigured", "configured", "configured") });
  }
  const view = render(tree());
  const input = screen.getByRole("textbox", { name: "Ask" });
  fireEvent.change(input, { target: { value: "Keep my question" } });
  // Quiet readiness: no status chatter / Open Fleet banners under the composer.
  expect(screen.queryByText(/Checking whether chat/)).toBeNull();
  expect(screen.queryByRole("status")).toBeNull();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Open Fleet" })).toBeNull();
  view.rerender(tree({ ...READY_ASK, state: "unavailable", message: "" }));
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement).value).toBe("Keep my question");
  expect(screen.queryByText(/No chat model is available/)).toBeNull();
  expect(screen.queryByRole("button", { name: "Open Fleet" })).toBeNull();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.submit(input.closest("form")!); expect(ask).not.toHaveBeenCalled();
  view.rerender(tree(READY_ASK));
  fireEvent.click(screen.getByRole("button", { name: "Send" })); expect(ask).toHaveBeenCalledOnce();
});

it("keeps the sheet draft across availability flips without Open Fleet banners", () => {
  function Flip() {
    const { openAsk } = useAsk();
    return <button onClick={() => openAsk()}>Return to Ask</button>;
  }
  const unavailable: AskAvailability = { ...READY_ASK, state: "unavailable", message: "" };
  render(withSession(withOs(
    <AskProvider transport={{ ask: vi.fn(() => ({ cancel: vi.fn() })) }} availability={unavailable}>
      <OpenOnMount /><Flip /><AskSheet />
    </AskProvider>, "owner")));
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Save this question while I connect a machine" } });
  expect(screen.queryByRole("button", { name: "Open Fleet" })).toBeNull();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Return to Ask" }));
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement).value).toBe("Save this question while I connect a machine");
});
