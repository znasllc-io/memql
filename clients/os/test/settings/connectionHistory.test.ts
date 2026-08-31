import { describe, expect, it } from "vitest";

import {
  EMPTY_HISTORY,
  HISTORY_LIMIT,
  recordTransition,
  type ConnectionHistory,
} from "../../src/apps/settings/connectionHistory";

function ev(status: "connected" | "reconnecting" | "disconnected", attempt = 0, error = "") {
  return { status, attempt, error };
}

describe("the connection transition buffer (memql#4744)", () => {
  it("marks the first event a baseline reading, not a transition", () => {
    const h = recordTransition(EMPTY_HISTORY, ev("connected"), 1000);
    expect(h.transitions).toHaveLength(1);
    expect(h.transitions[0]!.baseline).toBe(true);
    // The subscribe-time event tells you nothing about what came before, so
    // it can never be a reconnect. Counting it would tell an operator the
    // connection had just recovered at the moment they opened the window.
    expect(h.lastReconnectAt).toBeNull();
  });

  it("records a drop and the recovery, in order, with the reconnect time", () => {
    let h: ConnectionHistory = recordTransition(EMPTY_HISTORY, ev("connected"), 1000);
    h = recordTransition(h, ev("reconnecting", 1, "stream closed"), 2000);
    h = recordTransition(h, ev("reconnecting", 2, "stream closed"), 3000);
    h = recordTransition(h, ev("connected"), 4000);

    expect(h.transitions.map((t) => `${t.status}:${t.attempt}`)).toEqual([
      "connected:0",
      "reconnecting:1",
      "reconnecting:2",
      "connected:0",
    ]);
    expect(h.transitions[1]!.error).toBe("stream closed");
    expect(h.lastReconnectAt).toBe(4000);
    expect(h.current?.status).toBe("connected");
  });

  it("drops a repeat of an identical reading", () => {
    let h = recordTransition(EMPTY_HISTORY, ev("reconnecting", 3, "boom"), 1000);
    const before = h;
    h = recordTransition(h, ev("reconnecting", 3, "boom"), 2000);
    // Same object back: the SDK re-emits, and fifty slots spent on one line
    // is a buffer that cannot show you the drop that matters.
    expect(h).toBe(before);
    expect(h.transitions).toHaveLength(1);
  });

  it("keeps a rising attempt count -- that is progress, not a repeat", () => {
    let h = recordTransition(EMPTY_HISTORY, ev("reconnecting", 1), 1000);
    h = recordTransition(h, ev("reconnecting", 2), 2000);
    expect(h.transitions).toHaveLength(2);
  });

  it("bounds the buffer, keeping the newest", () => {
    let h = EMPTY_HISTORY;
    for (let i = 0; i < HISTORY_LIMIT + 20; i++) {
      h = recordTransition(h, ev("reconnecting", i + 1), i * 10);
    }
    expect(h.transitions).toHaveLength(HISTORY_LIMIT);
    expect(h.transitions[h.transitions.length - 1]!.attempt).toBe(HISTORY_LIMIT + 20);
    // The oldest are the ones dropped.
    expect(h.transitions[0]!.attempt).toBe(21);
  });

  it("does not count a first-ever connect as a reconnect, but does count a later one", () => {
    let h = recordTransition(EMPTY_HISTORY, ev("disconnected"), 1000);
    expect(h.lastReconnectAt).toBeNull();
    h = recordTransition(h, ev("connected"), 2000);
    expect(h.lastReconnectAt).toBe(2000);
  });
});
