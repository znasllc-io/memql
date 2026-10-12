import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ActionBar, type Act } from "../../src/kit/ActionBar";

// AN ASK AND ITS CONFIRMATION, ONE DOUBLE-CLICK APART (memql#6033).
//
// Send now asks first, in the bar, and names how many people it reaches; Cancel
// asks and is answered by Cancel campaign. The question's acts stand where the
// bar's acts stood, right-aligned, so which of them ends up under the pointer
// is a matter of widths -- a different label, or another place in the list, is
// no protection. Without a guard the second click of a double-click confirms a
// question nobody read. So a bar that turns into a question takes no press at
// all for a moment, whatever stands where; and an ask that comes back to its
// place is held the same way, so the rest of a double-click does not ask again.

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-12T09:00:00Z"));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const later = (ms: number) => vi.setSystemTime(new Date(Date.now() + ms));
const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));

/** Send now asks; its confirmation is Send now again, at the same place. */
function AskThenSend({ onSend, onTest = () => {} }: { onSend: () => void; onTest?: () => void }) {
  const [asking, setAsking] = useState(false);
  const acts: Act[] = asking
    ? [
        { label: "Not yet", text: true, onAct: () => setAsking(false) },
        { label: "Send now", tone: "primary", onAct: onSend },
      ]
    : [
        { label: "Send a test", text: true, onAct: onTest },
        { label: "Send now", tone: "primary", onAct: () => setAsking(true) },
      ];
  return (
    <ActionBar state="Ready to send" acts={acts} asking={asking}>
      {asking ? <span className="os-actbar-confirm">Send “August update” to 1,204 subscribers now?</span> : null}
    </ActionBar>
  );
}

/** Archive asks from the left; its confirmation stands on the right edge. */
function ArchiveFromTheLeft({ onArchive, onKeep = () => {} }: { onArchive: () => void; onKeep?: () => void }) {
  const [asking, setAsking] = useState(false);
  const acts: Act[] = asking
    ? [
        { label: "Keep", text: true, onAct: () => { onKeep(); setAsking(false); } },
        { label: "Archive", tone: "danger", onAct: onArchive },
      ]
    : [
        { label: "Archive", text: true, onAct: () => setAsking(true) },
        { label: "Edit", text: true, onAct: () => {} },
      ];
  return <ActionBar state="Active" acts={acts} asking={asking}>{asking ? <span>Archive news@acme.com?</span> : null}</ActionBar>;
}

describe("a confirmation that takes the place of its ask", () => {
  it("is not pressed by the second click of the double-click that asked", () => {
    const send = vi.fn();
    render(<AskThenSend onSend={send} />);
    press("Send now");
    expect(screen.getByText("Send “August update” to 1,204 subscribers now?")).toBeTruthy();
    press("Send now");
    expect(send).not.toHaveBeenCalled();
    // Still asking: the press that landed too soon neither confirmed nor cancelled.
    expect(screen.getByText("Send “August update” to 1,204 subscribers now?")).toBeTruthy();
  });

  it("ignores presses for 400 ms after it appears, then takes the next one", () => {
    const send = vi.fn();
    render(<AskThenSend onSend={send} />);
    press("Send now");
    later(399);
    press("Send now");
    expect(send).not.toHaveBeenCalled();
    later(1);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("counts the moment from when it appears, every time it is asked again", () => {
    const send = vi.fn();
    render(<AskThenSend onSend={send} />);
    press("Send now");
    later(2_000);
    press("Not yet");
    later(2_000);
    // Asked again: the new confirmation is held from ITS appearance, not the first one's.
    press("Send now");
    press("Send now");
    expect(send).not.toHaveBeenCalled();
    later(400);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });
});

/** The campaign page's Cancel: a lone text act answered by Keep and Cancel campaign. */
function CancelAsks({ before = [], onCancel, onKeep = () => {} }: { before?: Act[]; onCancel: () => void; onKeep?: () => void }) {
  const [asking, setAsking] = useState(false);
  const acts: Act[] = asking
    ? [
        { label: "Keep", text: true, onAct: () => { onKeep(); setAsking(false); } },
        { label: "Cancel campaign", tone: "danger", onAct: onCancel },
      ]
    : [{ label: "Cancel", text: true, onAct: () => setAsking(true) }, ...before];
  return <ActionBar state="Scheduled" acts={acts} asking={asking}>{asking ? <span className="os-actbar-confirm">Cancel “Launch day”?</span> : null}</ActionBar>;
}

describe("a question that replaces its ask under other labels", () => {
  it("is not answered by the second click of the double-click that asked", () => {
    // [Cancel] becomes [Keep, Cancel campaign]: the confirmation stands on the
    // right edge, exactly where Cancel stood, under another label.
    const cancel = vi.fn();
    render(<CancelAsks onCancel={cancel} />);
    press("Cancel");
    press("Cancel campaign");
    expect(cancel).not.toHaveBeenCalled();
    expect(screen.getByText("Cancel “Launch day”?")).toBeTruthy();
    later(400);
    press("Cancel campaign");
    expect(cancel).toHaveBeenCalledTimes(1);
  });

  it("takes no press at all for a moment where it only partly covers what stood there", () => {
    // [Cancel, Send now] becomes [Keep, Cancel campaign]: wider acts, other
    // places -- whichever one the second click lands on, it does nothing.
    const cancel = vi.fn();
    const keep = vi.fn();
    render(<CancelAsks before={[{ label: "Send now", tone: "primary", onAct: () => {} }]} onCancel={cancel} onKeep={keep} />);
    press("Cancel");
    press("Cancel campaign");
    press("Keep");
    expect(cancel).not.toHaveBeenCalled();
    expect(keep).not.toHaveBeenCalled();
    expect(screen.getByText("Cancel “Launch day”?")).toBeTruthy();
    later(400);
    press("Keep");
    expect(keep).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Cancel “Launch day”?")).toBeNull();
  });
});

describe("the way back", () => {
  it("is held too when a refused confirmation puts its ask back in that place", () => {
    // The confirmation is refused at once and the bar asks again; the rest of
    // the double-click on the confirmation does not open the question again.
    function RefusedAtOnce() {
      const [asking, setAsking] = useState(false);
      const acts: Act[] = asking
        ? [
            { label: "Not yet", text: true, onAct: () => setAsking(false) },
            { label: "Send now", tone: "primary", onAct: () => setAsking(false) },
          ]
        : [
            { label: "Cancel", text: true, onAct: () => {} },
            { label: "Send now", tone: "primary", onAct: () => setAsking(true) },
          ];
      return <ActionBar state="Draft" acts={acts} asking={asking}>{asking ? <span>Send now?</span> : null}</ActionBar>;
    }
    render(<RefusedAtOnce />);
    press("Send now");
    later(400);
    press("Send now");
    expect(screen.queryByText("Send now?")).toBeNull();
    press("Send now");
    expect(screen.queryByText("Send now?")).toBeNull();
    later(400);
    press("Send now");
    expect(screen.getByText("Send now?")).toBeTruthy();
  });
});

describe("a question that asks from somewhere else", () => {
  it("holds the confirmation even where it stands at another place than its ask", () => {
    // Archive asks from the left and is confirmed from the right edge: the
    // places differ, but a place counts acts, not pixels.
    const archive = vi.fn();
    const keep = vi.fn();
    render(<ArchiveFromTheLeft onArchive={archive} onKeep={keep} />);
    press("Archive");
    press("Archive");
    press("Keep");
    expect(archive).not.toHaveBeenCalled();
    expect(keep).not.toHaveBeenCalled();
    later(400);
    press("Archive");
    expect(archive).toHaveBeenCalledTimes(1);
  });
});

describe("what the guard leaves alone", () => {
  it("an act that stays where it is answers every press", () => {
    const test = vi.fn();
    render(<AskThenSend onSend={() => {}} onTest={test} />);
    press("Send a test");
    press("Send a test");
    expect(test).toHaveBeenCalledTimes(2);
  });

  it("an act that stays put answers every press, even just after the bar changed around it", () => {
    const test = vi.fn();
    function Grows({ drafted }: { drafted: boolean }) {
      const acts: Act[] = [
        ...(drafted ? [{ label: "Cancel", text: true, onAct: () => {} }] : []),
        { label: "Send a test", text: true, onAct: test },
        { label: "Send now", tone: "primary", onAct: () => {} },
      ];
      return <ActionBar state="Draft" acts={acts} />;
    }
    const view = render(<Grows drafted={false} />);
    view.rerender(<Grows drafted />);
    press("Send a test");
    press("Send a test");
    expect(test).toHaveBeenCalledTimes(2);
  });

  it("another act that takes the place of the one pressed is not held: it is a different promise", () => {
    const resume = vi.fn();
    function PauseResume() {
      const [paused, setPaused] = useState(false);
      const acts: Act[] = paused
        ? [{ label: "Resume", tone: "primary", onAct: resume }]
        : [{ label: "Pause", tone: "primary", onAct: () => setPaused(true) }];
      return <ActionBar state={paused ? "Paused" : "Sending"} acts={acts} />;
    }
    render(<PauseResume />);
    press("Pause");
    press("Resume");
    expect(resume).toHaveBeenCalledTimes(1);
  });


  it("a wizard's next step is a new question, not the answer to the last one", () => {
    // Next on one step, Next on the step it opens: the same label at the same
    // place, but the bar moved to another step -- nothing was asked.
    const next = vi.fn();
    function TwoSteps() {
      const [step, setStep] = useState("address");
      const acts: Act[] =
        step === "address"
          ? [{ label: "Next", tone: "primary", onAct: () => setStep("name") }]
          : [
              { label: "Back", text: true, onAct: () => setStep("address") },
              { label: "Next", tone: "primary", onAct: next },
            ];
      return <ActionBar state="Your turn" acts={acts} scope={step} />;
    }
    render(<TwoSteps />);
    press("Next");
    press("Next");
    expect(next).toHaveBeenCalledTimes(1);
  });

  it("a bar that draws something else beside its acts is not a question", () => {
    // A refusal drawn in the bar beside acts that changed: nothing was asked,
    // so the act that now stands there answers the very next press.
    const retry = vi.fn();
    function Refused() {
      const [failed, setFailed] = useState(false);
      const acts: Act[] = failed
        ? [{ label: "Try again", tone: "primary", onAct: retry }]
        : [{ label: "Run", tone: "primary", onAct: () => setFailed(true) }];
      return <ActionBar state="Ready" acts={acts}>{failed ? <span role="alert">The run was refused.</span> : null}</ActionBar>;
    }
    render(<Refused />);
    press("Run");
    press("Try again");
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("holds nothing on a bar nobody has pressed yet", () => {
    const send = vi.fn();
    render(<ActionBar state="Ready to send" acts={[{ label: "Send now", tone: "primary", onAct: send }]} />);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });
});
