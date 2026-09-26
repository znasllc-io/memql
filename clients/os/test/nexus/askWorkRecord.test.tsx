import { READY_ASK } from "../../src/ask/useAskReadiness";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskTransport } from "../../src/ask/askController";

describe("Ask's shared work record", () => {
  it("keeps a completed reply free of the retired per-message work menu", async () => {
    const ask = vi.fn<AskTransport["ask"]>((_prompt, _context, callbacks) => {
      callbacks.activity?.({ id: "run-event", kind: "run", phase: "running", at: new Date().toISOString(), arguments: { goalId: "existing-goal", runId: "existing-run" } });
      callbacks.delta("Done."); callbacks.done();
      return { cancel() {} };
    });
    render(<AskSurface availability={READY_ASK} transport={{ ask }} variant="sheet" />);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "Open Fleet" } });
    fireEvent.submit(screen.getByRole("textbox").closest("form")!);
    expect(await screen.findByText("Done.")).toBeTruthy();
    expect(screen.queryByText("View work")).toBeNull();
    expect(ask).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Make this a goal")).toBeNull();
  });
});
