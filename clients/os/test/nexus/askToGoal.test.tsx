import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AskSurface } from "../../src/ask/AskSurface";
import { StubAskTransport } from "../../src/ask/askController";
import type { MakeGoalState } from "../../src/ask/useMakeGoal";

// ASK-TO-GOAL: the handoff from asking to having it done.
//
// The hook itself (`useMakeGoal`) is wiring over the shell's own openApp and
// the generated `createGoal`; what is worth pinning here is the SURFACE
// contract -- when the act appears, when it does not, and what it says.

function stub(over: Partial<MakeGoalState> = {}): MakeGoalState {
  return { busy: false, error: "", make: vi.fn(async () => {}), reset: vi.fn(), ...over };
}

function mount(makeGoal: MakeGoalState | null) {
  return render(
    <AskSurface transport={new StubAskTransport()} variant="sheet" makeGoal={makeGoal} />,
  );
}

async function ask(text: string) {
  fireEvent.change(screen.getByRole("textbox"), { target: { value: text } });
  fireEvent.submit(screen.getByRole("textbox").closest("form")!);
  await waitFor(() => expect(screen.getByText(text)).toBeTruthy());
}

describe("the handoff", () => {
  it("offers to make the prompt a goal once the answer has landed", async () => {
    mount(stub());
    await ask("Reconcile the September ledger");
    await waitFor(() => expect(screen.getByText("Make this a goal")).toBeTruthy());
  });

  it("hands the PROMPT over, not the answer", async () => {
    const makeGoal = stub();
    mount(makeGoal);
    await ask("Reconcile the September ledger");
    fireEvent.click(await screen.findByText("Make this a goal"));
    expect(makeGoal.make).toHaveBeenCalledWith("Reconcile the September ledger");
  });

  // ABSENT, not disabled: a surface with no cluster behind it should show the
  // Ask it actually has rather than an act that cannot work.
  it("is not offered at all when this surface cannot hand anything off", async () => {
    mount(null);
    await ask("Reconcile the September ledger");
    expect(screen.queryByText("Make this a goal")).toBeNull();
  });

  it("says what it is doing while it is doing it", async () => {
    mount(stub({ busy: true }));
    await ask("Reconcile the September ledger");
    const control = await screen.findByText("Making it a goal");
    expect((control as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows a refusal in the server's own words", async () => {
    mount(stub({ error: "PERMISSION_DENIED: goals are not enabled here" }));
    await ask("Reconcile the September ledger");
    expect(screen.getByRole("alert").textContent).toContain(
      "PERMISSION_DENIED: goals are not enabled here",
    );
  });
});
