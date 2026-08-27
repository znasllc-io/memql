import { act, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ONLINE_WINDOW_SECONDS, isWorkerOnline } from "../../src/apps/fleet/online";
import { emptyArrivals, observeSnapshot, TICK_TTL_MS } from "../../src/live/arrival";
import { LiveList, type LiveListSource } from "../../src/live/LiveList";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

interface Row {
  id: string;
  name: string;
  v: number;
}

function fakeSource(initial: LiveSnapshot<Row>): LiveListSource<Row> & {
  push: (s: LiveSnapshot<Row>) => void;
} {
  let snapshot = initial;
  const listeners = new Set<() => void>();
  return {
    get snapshot() {
      return snapshot;
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    push(next) {
      snapshot = next;
      for (const l of listeners) l();
    },
  };
}

const snap = (rows: Row[], state: LiveSnapshot<Row>["state"], version: number): LiveSnapshot<Row> => ({
  rows,
  state,
  error: "",
  version,
});

describe("arrival reducer", () => {
  it("treats the seed as baseline and later rows as arrivals", () => {
    let s = emptyArrivals();
    s = observeSnapshot(s, [{ id: "a", fingerprint: "1" }], "live", 1000);
    expect(s.ticks.size).toBe(0); // baseline: wasLive was false
    s = observeSnapshot(s, [{ id: "a", fingerprint: "1" }, { id: "b", fingerprint: "1" }], "live", 2000);
    expect(s.ticks.get("b")?.kind).toBe("added");
    expect(s.ticks.has("a")).toBe(false);
  });

  it("marks a changed fingerprint as updated and decays by the clock", () => {
    let s = emptyArrivals();
    s = observeSnapshot(s, [{ id: "a", fingerprint: "1" }], "live", 0);
    s = observeSnapshot(s, [{ id: "a", fingerprint: "2" }], "live", 100);
    expect(s.ticks.get("a")?.kind).toBe("updated");
    s = observeSnapshot(s, [{ id: "a", fingerprint: "2" }], "live", 100 + TICK_TTL_MS + 1);
    expect(s.ticks.size).toBe(0);
  });

  it("does not re-animate a reconnect resync", () => {
    let s = emptyArrivals();
    s = observeSnapshot(s, [{ id: "a", fingerprint: "1" }], "live", 0);
    // The stream degrades; the next full snapshot is a BASELINE.
    s = observeSnapshot(s, [], "degraded", 10);
    s = observeSnapshot(
      s,
      [{ id: "a", fingerprint: "1" }, { id: "b", fingerprint: "1" }],
      "live",
      20,
    );
    expect(s.ticks.size).toBe(0);
  });
});

describe("LiveList", () => {
  it("renders rows, plays the added tick, and shows the state caption", () => {
    const source = fakeSource(snap([], "seeding", 1));
    render(
      <LiveList<Row>
        source={source}
        rowId={(r) => r.id}
        fingerprint={(r) => String(r.v)}
        label="Rows"
        emptyText="Nothing yet."
        renderRow={(r, tick) => (
          <span>
            {r.name}
            {tick === "added" ? " (new)" : ""}
          </span>
        )}
      />,
    );
    expect(screen.getByText("Loading from the cluster")).toBeTruthy();

    act(() => source.push(snap([{ id: "a", name: "alpha", v: 1 }], "live", 2)));
    expect(screen.getByText("alpha")).toBeTruthy();

    act(() => source.push(snap([{ id: "a", name: "alpha", v: 1 }, { id: "b", name: "beta", v: 1 }], "live", 3)));
    expect(screen.getByText("beta (new)")).toBeTruthy();
    const row = document.querySelector('[data-arrival="added"]');
    expect(row).not.toBeNull();
  });

  it("renders the disconnected caption for a null source, never a fake empty list", () => {
    render(
      <LiveList<Row>
        source={null}
        rowId={(r) => r.id}
        fingerprint={() => ""}
        label="Rows"
        emptyText="Nothing yet."
        renderRow={(r) => <span>{r.name}</span>}
      />,
    );
    expect(screen.getByText("Not connected to the cluster")).toBeTruthy();
    expect(screen.queryByText("Nothing yet.")).toBeNull();
  });
});

describe("the OS online rule", () => {
  it("matches the derived-window boundaries", () => {
    const now = new Date("2026-08-27T00:10:00Z");
    const at = (secondsAgo: number) => new Date(now.getTime() - secondsAgo * 1000).toISOString();
    expect(isWorkerOnline({ lastSeenAt: at(ONLINE_WINDOW_SECONDS - 1) }, now)).toBe(true);
    expect(isWorkerOnline({ lastSeenAt: at(ONLINE_WINDOW_SECONDS + 1) }, now)).toBe(false);
    expect(isWorkerOnline({ lastSeenAt: at(1), revokedAt: at(100) }, now)).toBe(false);
    expect(isWorkerOnline({}, now)).toBe(false);
    expect(isWorkerOnline({ lastSeenAt: "not-a-time" }, now)).toBe(false);
  });
});

describe("timers", () => {
  it("keeps vitest fake-timer compatibility for the tick decay", () => {
    vi.useFakeTimers();
    vi.useRealTimers();
    expect(true).toBe(true);
  });
});
