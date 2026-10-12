import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ActionBar, type Act } from "../../src/kit/ActionBar";

// A BAR THAT CHANGES, ONE DOUBLE-CLICK AFTER IT WAS PRESSED (memql#6033).
//
// Send now asks first, in the bar, and names how many people it reaches; Cancel
// asks and is answered by Cancel campaign; Keep goes back to a bar that offers
// Discard changes; Next opens a step whose Next stands where the last one did.
// The acts are right-aligned, so which of them ends up under the pointer is a
// matter of widths -- a different label, or another place in the list, is no
// protection. Without a guard the second click of a double-click presses
// whatever the first one put there. So ANY change of what a bar offers holds
// every press for a moment, whatever stands where, on every bar in the shell.

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
    <ActionBar state="Ready to send" acts={acts}>
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
  return <ActionBar state="Active" acts={acts}>{asking ? <span>Archive news@acme.com?</span> : null}</ActionBar>;
}

/** The campaign page's Cancel: a lone text act answered by Keep and Cancel campaign. */
function CancelAsks({ before = [], onCancel, onKeep = () => {} }: { before?: Act[]; onCancel: () => void; onKeep?: () => void }) {
  const [asking, setAsking] = useState(false);
  const acts: Act[] = asking
    ? [
        { label: "Keep", text: true, onAct: () => { onKeep(); setAsking(false); } },
        { label: "Cancel campaign", tone: "danger", onAct: onCancel },
      ]
    : [{ label: "Cancel", text: true, onAct: () => setAsking(true) }, ...before];
  return <ActionBar state="Scheduled" acts={acts}>{asking ? <span className="os-actbar-confirm">Cancel “Launch day”?</span> : null}</ActionBar>;
}

describe("a question that takes the place of its ask", () => {
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

  it("is not answered under other labels: [Cancel] becomes [Keep, Cancel campaign]", () => {
    // The confirmation stands on the right edge, exactly where Cancel stood,
    // under another label.
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

describe("the way back", () => {
  it("a double-click on Keep beside unsaved changes does not discard them", () => {
    // [Cancel campaign, Discard changes, Save] asks as [Keep, Cancel campaign].
    // Keep puts the edit bar back, and Discard changes now stands where Keep
    // stood: the rest of the double-click on Keep must not throw the edits away.
    const discard = vi.fn();
    function EditWithChanges() {
      const [asking, setAsking] = useState(false);
      const acts: Act[] = asking
        ? [
            { label: "Keep", text: true, onAct: () => setAsking(false) },
            { label: "Cancel campaign", tone: "danger", onAct: () => {} },
          ]
        : [
            { label: "Cancel campaign", text: true, tone: "danger", onAct: () => setAsking(true) },
            { label: "Discard changes", text: true, onAct: discard },
            { label: "Save", tone: "primary", onAct: () => {} },
          ];
      return <ActionBar state="Editing" acts={acts}>{asking ? <span>Cancel “August update”?</span> : null}</ActionBar>;
    }
    render(<EditWithChanges />);
    press("Cancel campaign");
    later(2_000);
    press("Keep");
    press("Discard changes");
    expect(discard).not.toHaveBeenCalled();
    later(400);
    press("Discard changes");
    expect(discard).toHaveBeenCalledTimes(1);
  });

  it("is held when a refused confirmation puts its ask back in that place", () => {
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
      return <ActionBar state="Draft" acts={acts}>{asking ? <span>Send now?</span> : null}</ActionBar>;
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

describe("every bar, not only a question", () => {
  it("a double-click on Pause does not resume", () => {
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
    expect(resume).not.toHaveBeenCalled();
    later(400);
    press("Resume");
    expect(resume).toHaveBeenCalledTimes(1);
  });

  it("a refusal that changes the acts holds them like any change", () => {
    // Run is refused at once and Try again takes its place, an error beside it:
    // the rest of the double-click on Run does not run it again.
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
    expect(retry).not.toHaveBeenCalled();
    later(400);
    press("Try again");
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("an act that stayed where it was waits a moment too when the bar changed around it", () => {
    // Nothing pressed changed it -- a draft arrived and Cancel joined the bar --
    // but what stands where is no longer what the person aimed at.
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
    expect(test).not.toHaveBeenCalled();
    later(400);
    press("Send a test");
    press("Send a test");
    expect(test).toHaveBeenCalledTimes(2);
  });

  it("a double-click on Next does not answer the step it opens", () => {
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
    expect(next).not.toHaveBeenCalled();
    later(400);
    press("Next");
    expect(next).toHaveBeenCalledTimes(1);
  });

  it("even when the step it opens offers the same acts: the step is what the bar is about", () => {
    // [Back, Next] on one step, [Back, Next] on the next: the labels did not
    // move, but the bar now asks about another step.
    const finish = vi.fn();
    function SameActs() {
      const [step, setStep] = useState(1);
      const acts: Act[] = [
        { label: "Back", text: true, onAct: () => setStep((s) => s - 1) },
        { label: "Next", tone: "primary", onAct: () => (step === 3 ? finish() : setStep((s) => s + 1)) },
      ];
      return <ActionBar state={`Step ${step}`} acts={acts} scope={`step-${step}`} />;
    }
    render(<SameActs />);
    press("Next");
    press("Next");
    expect(within(bar()).getByText("Step 2")).toBeTruthy();
    later(400);
    press("Next");
    press("Next");
    expect(within(bar()).getByText("Step 3")).toBeTruthy();
    expect(finish).not.toHaveBeenCalled();
  });
});

describe("what the guard leaves alone", () => {
  it("an act whose bar did not change answers every press", () => {
    const test = vi.fn();
    render(<AskThenSend onSend={() => {}} onTest={test} />);
    press("Send a test");
    press("Send a test");
    expect(test).toHaveBeenCalledTimes(2);
  });

  it("a bar whose acts and what it draws stay the same answers at once, however often it renders", () => {
    const run = vi.fn();
    function SaysWhy({ refusal }: { refusal: string }) {
      return (
        <ActionBar state="Ready" acts={[{ label: "Run", tone: "primary", onAct: run }]}>
          {refusal === "" ? null : <span role="alert">{refusal}</span>}
        </ActionBar>
      );
    }
    const view = render(<SaysWhy refusal="The run was refused." />);
    view.rerender(<SaysWhy refusal="The run was refused." />);
    press("Run");
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("holds nothing on a bar nobody has pressed yet", () => {
    const send = vi.fn();
    render(<ActionBar state="Ready to send" acts={[{ label: "Send now", tone: "primary", onAct: send }]} />);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("takes a bar's first acts at once: nothing stood there to be pressed", () => {
    // A page whose record arrives after it opens draws its bar first with the
    // state alone; the acts that appear are the first anything offered there.
    const send = vi.fn();
    function Loads({ loaded }: { loaded: boolean }) {
      return <ActionBar state={loaded ? "Draft" : "Reading"} acts={loaded ? [{ label: "Send now", tone: "primary", onAct: send }] : []} />;
    }
    const view = render(<Loads loaded={false} />);
    view.rerender(<Loads loaded />);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("acts that come back after a moment without any are a change only when they are other acts", () => {
    // A write in flight takes the acts away; the same ones coming back are not
    // new, and other ones are -- the bar remembers what it last offered.
    const pause = vi.fn();
    const resume = vi.fn();
    function Writes({ phase }: { phase: "sending" | "writing" | "paused" }) {
      const acts: Act[] =
        phase === "writing" ? [] : phase === "sending" ? [{ label: "Pause", tone: "primary", onAct: pause }] : [{ label: "Resume", tone: "primary", onAct: resume }];
      return <ActionBar state={phase} acts={acts} />;
    }
    const view = render(<Writes phase="sending" />);
    view.rerender(<Writes phase="writing" />);
    view.rerender(<Writes phase="sending" />);
    press("Pause");
    expect(pause).toHaveBeenCalledTimes(1);

    view.rerender(<Writes phase="writing" />);
    view.rerender(<Writes phase="paused" />);
    press("Resume");
    expect(resume).not.toHaveBeenCalled();
    later(400);
    press("Resume");
    expect(resume).toHaveBeenCalledTimes(1);
  });
});

describe("everything pressable in a bar, not only its acts", () => {
  it("holds an ask drawn as the bar's children: the Files Inspector's Delete", () => {
    // No acts at all: the bar draws its own buttons, and Delete turns them into
    // [Cancel, Delete] -- the confirmation on the right edge where Delete stood.
    const remove = vi.fn();
    function DrawnAsk() {
      const [asking, setAsking] = useState(false);
      return (
        <ActionBar state={asking ? "Delete “report.pdf”?" : "In Library"}>
          {asking ? (
            <>
              <button type="button" className="os-actbar-text" onClick={() => setAsking(false)}>Cancel</button>
              <button type="button" className="os-button" data-tone="danger" onClick={remove}>Delete</button>
            </>
          ) : (
            <button type="button" className="os-button" data-tone="danger" aria-label="Delete" onClick={() => setAsking(true)}>x</button>
          )}
        </ActionBar>
      );
    }
    render(<DrawnAsk />);
    press("Delete");
    press("Delete");
    expect(remove).not.toHaveBeenCalled();
    expect(within(bar()).getByText("Delete “report.pdf”?")).toBeTruthy();
    later(400);
    press("Delete");
    expect(remove).toHaveBeenCalledTimes(1);
  });

  it("holds what a child changes on its own, without the bar rendering again", async () => {
    // A confirmation row that keeps its own state (DeployablePage's): the bar is
    // not rendered again when it turns into a question, and is held all the same.
    const remove = vi.fn();
    function ConfirmRow() {
      const [asking, setAsking] = useState(false);
      return asking ? (
        <>
          <span>Remove shop.example.com?</span>
          <button type="button" className="os-actbar-text" onClick={() => setAsking(false)}>Keep it</button>
          <button type="button" className="os-button" data-tone="danger" onClick={remove}>Remove</button>
        </>
      ) : (
        <button type="button" className="os-button" onClick={() => setAsking(true)}>Remove</button>
      );
    }
    const row = <ConfirmRow />;
    render(<ActionBar state="Serving">{row}</ActionBar>);
    press("Remove");
    press("Remove");
    expect(remove).not.toHaveBeenCalled();
    expect(within(bar()).getByText("Remove shop.example.com?")).toBeTruthy();
    // The change is noticed when it happens, not when it is next pressed: once
    // read, the confirmation answers.
    await act(async () => {});
    later(400);
    press("Remove");
    expect(remove).toHaveBeenCalledTimes(1);
  });

  it("holds a refusal that appears beside unchanged acts: what a bar draws is part of what it offers", () => {
    const run = vi.fn();
    function SaysWhy({ refusal }: { refusal: string }) {
      return (
        <ActionBar state="Ready" acts={[{ label: "Run", tone: "primary", onAct: run }]}>
          {refusal === "" ? null : <span role="alert">{refusal}</span>}
        </ActionBar>
      );
    }
    const view = render(<SaysWhy refusal="" />);
    view.rerender(<SaysWhy refusal="The run was refused." />);
    press("Run");
    expect(run).not.toHaveBeenCalled();
    later(400);
    press("Run");
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("holds the keyboard too: Enter pressed twice on an ask does not answer it", () => {
    // A browser turns Enter on a focused button into a click (detail 0), which
    // jsdom does not; this is that click, after the key that made it.
    const enter = (name: string) => {
      const button = within(bar()).getByRole("button", { name });
      button.focus();
      fireEvent.keyDown(button, { key: "Enter" });
      fireEvent.click(button, { detail: 0 });
    };
    const send = vi.fn();
    render(<AskThenSend onSend={send} />);
    enter("Send now");
    enter("Send now");
    expect(send).not.toHaveBeenCalled();
    expect(screen.getByText("Send “August update” to 1,204 subscribers now?")).toBeTruthy();
    later(400);
    enter("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("holds an ask that only changes how its act is dressed", () => {
    // Archive turns into its own confirmation in place: the same label, now the
    // danger button rather than a quiet one.
    const archive = vi.fn();
    function Retoned() {
      const [asking, setAsking] = useState(false);
      return (
        <ActionBar
          state={asking ? "Archive it?" : "Active"}
          acts={[{ label: "Archive", tone: asking ? "danger" : "quiet", onAct: asking ? archive : () => setAsking(true) }]}
        />
      );
    }
    render(<Retoned />);
    press("Archive");
    press("Archive");
    expect(archive).not.toHaveBeenCalled();
    later(400);
    press("Archive");
    expect(archive).toHaveBeenCalledTimes(1);
  });

  it("is not held forever by a clock that steps back", () => {
    const send = vi.fn();
    render(<AskThenSend onSend={send} />);
    press("Send now");
    // The machine's clock is set back a minute: the bar is not held for a minute.
    later(-60_000);
    press("Send now");
    expect(send).toHaveBeenCalledTimes(1);
  });
});

describe("a text act that cannot be undone", () => {
  it("reads as one, not as the muted way out", () => {
    // "Cancel campaign" on an Edit page is a label beside the button, and a
    // muted label reads as the way out -- this one is the opposite of that.
    render(<ActionBar state="Editing" acts={[{ label: "Cancel campaign", text: true, tone: "danger", onAct: () => {} }]} />);
    expect(within(bar()).getByRole("button", { name: "Cancel campaign" }).getAttribute("data-tone")).toBe("danger");
    const css = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");
    expect(css).toContain('.os-actbar-text[data-tone="danger"] {\n  color: var(--os-error);');
  });
});
