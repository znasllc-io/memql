import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { CONFIRM_SETTLE_MS, InPlaceAsk } from "../../src/kit/pressHold";

// A QUESTION ANSWERED IN PLACE, ONE DOUBLE-CLICK AFTER IT WAS ASKED (memql#6033).
//
// A row's Revoke becomes "Revoke it / Keep it" where it stood; a panel's
// Disconnect expands into Cancel and a confirmation. They are outside any bar,
// so the bar's hold did not cover them, and the second click of the
// double-click that asked answered a question nobody had read. `InPlaceAsk` is
// the same hold the bar uses (kit/pressHold), around the question and its
// buttons.

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-12T09:00:00Z"));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const later = (ms: number) => vi.setSystemTime(new Date(Date.now() + ms));
const press = (name: string) => fireEvent.click(screen.getByRole("button", { name }));

function RevokeRow({
  onRevoke,
  onKeep = () => {},
  onSelect = () => {},
  as,
}: {
  onRevoke: () => void;
  onKeep?: () => void;
  onSelect?: () => void;
  as?: "div" | "span";
}) {
  const [asking, setAsking] = useState(false);
  const [refused, setRefused] = useState(false);
  const [reason, setReason] = useState("");
  return (
    <div onClick={onSelect}>
      <button type="button" onClick={() => setRefused(true)}>
        Refuse it
      </button>
      {asking ? (
        <InPlaceAsk as={as} className="os-ask-under-test" role="group" aria-label="Revoke Laptop">
          <p>Revoke Laptop? It stops working at once.</p>
          {refused ? <p role="alert">The cluster refused that.</p> : null}
          <input aria-label="Reason" value={reason} onChange={(event) => setReason(event.target.value)} />
          <button type="button" onClick={onRevoke}>
            Revoke it
          </button>
          <button
            type="button"
            onClick={() => {
              onKeep();
              setAsking(false);
            }}
          >
            Keep it
          </button>
        </InPlaceAsk>
      ) : (
        <button type="button" onClick={() => setAsking(true)}>
          Revoke
        </button>
      )}
    </div>
  );
}

describe("a question answered in place", () => {
  it("is not answered by the second click of the double-click that asked it", () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    expect(screen.getByText("Revoke Laptop? It stops working at once.")).toBeTruthy();
    later(50);
    press("Revoke it");
    expect(revoke).not.toHaveBeenCalled();
    // Still asking: the press that landed too soon neither answered nor withdrew it.
    expect(screen.getByRole("button", { name: "Revoke it" })).toBeTruthy();
  });

  it(`ignores presses for ${CONFIRM_SETTLE_MS} ms after it appears, then takes the next one`, () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    later(CONFIRM_SETTLE_MS - 1);
    press("Revoke it");
    expect(revoke).not.toHaveBeenCalled();
    later(1);
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it("holds the way out too: Keep it is not pressed by the click that asked", () => {
    const keep = vi.fn();
    render(<RevokeRow onRevoke={() => {}} onKeep={keep} />);
    press("Revoke");
    press("Keep it");
    expect(keep).not.toHaveBeenCalled();
    expect(screen.getByRole("group", { name: "Revoke Laptop" })).toBeTruthy();
    later(CONFIRM_SETTLE_MS);
    press("Keep it");
    expect(keep).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("group", { name: "Revoke Laptop" })).toBeNull();
  });

  it("counts the moment from when it appears, every time it is asked again", () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    later(2_000);
    press("Keep it");
    later(2_000);
    // Asked again: the new question is held from ITS appearance, not the first one's.
    press("Revoke");
    press("Revoke it");
    expect(revoke).not.toHaveBeenCalled();
    later(CONFIRM_SETTLE_MS);
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it("does not let a press it swallowed reach what stands behind it", () => {
    const select = vi.fn();
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} onSelect={select} />);
    press("Revoke");
    select.mockClear();
    press("Revoke it");
    // The click that was held opened nothing, selected nothing, answered nothing.
    expect(revoke).not.toHaveBeenCalled();
    expect(select).not.toHaveBeenCalled();
    later(CONFIRM_SETTLE_MS);
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
    expect(select).toHaveBeenCalledTimes(1);
  });

  it("holds again when something in it changes: a refusal drawn beside the buttons", () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    later(2_000);
    // The question has been read; then the cluster answers, and the buttons stand
    // beside a sentence nobody has read.
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Refuse it" }));
    });
    expect(screen.getByRole("alert").textContent).toBe("The cluster refused that.");
    press("Revoke it");
    expect(revoke).not.toHaveBeenCalled();
    later(CONFIRM_SETTLE_MS);
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it("is not held again by typing into a field it draws", () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    later(CONFIRM_SETTLE_MS - 100);
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "lost on a train" } });
    later(100);
    // What was typed is the person acting on the question, not the question changing.
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it("holds nothing when the clock is set back", () => {
    const revoke = vi.fn();
    render(<RevokeRow onRevoke={revoke} />);
    press("Revoke");
    later(-60_000);
    press("Revoke it");
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it("is the caller's element, class, role and label, and adds nothing else", () => {
    render(<RevokeRow onRevoke={() => {}} as="span" />);
    press("Revoke");
    const ask = screen.getByRole("group", { name: "Revoke Laptop" });
    expect(ask.tagName).toBe("SPAN");
    expect(ask.className).toBe("os-ask-under-test");
  });
});
