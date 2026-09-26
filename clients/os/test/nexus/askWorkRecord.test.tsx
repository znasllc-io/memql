import { READY_ASK } from "../../src/ask/useAskReadiness";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskTransport } from "../../src/ask/askController";

describe("Ask's shared work record", () => {
  it("opens the existing goal without submitting the request again", async () => {
    const open = vi.fn();
    const ask = vi.fn<AskTransport["ask"]>((_prompt, _context, callbacks) => {
      callbacks.activity?.({ id: "run-event", kind: "run", phase: "running", at: new Date().toISOString(), arguments: { goalId: "existing-goal", runId: "existing-run" } });
      callbacks.delta("Done."); callbacks.done();
      return { cancel() {} };
    });
    render(<AskSurface availability={READY_ASK} transport={{ ask }} variant="sheet" onOpenRun={open} />);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "Open Fleet" } });
    fireEvent.submit(screen.getByRole("textbox").closest("form")!);
    fireEvent.click(await screen.findByText("View work"));
    expect(open).toHaveBeenCalledWith("existing-goal");
    expect(ask).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Make this a goal")).toBeNull();
  });
});
