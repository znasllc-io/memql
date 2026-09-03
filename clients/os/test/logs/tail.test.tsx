import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../src/logs/AppLogsSection";
import { click, fakeConnection, logRow, logRows, withSession } from "./harness";

// The polling tail (epic memql#4895, spec L6): a baseline with no cursor,
// then `logsTail` with the newest row's occurredAt and id every two seconds,
// a re-baseline on a facet change, and a count that accumulates while the
// reader is scrolled up.

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  h.connection = null;
});

/** Let the pending reads resolve under fake timers. */
async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  });
}

async function elapse(ms: number): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
  await flush();
}

describe("the tail", () => {
  it("baselines with no cursor, then polls with the newest row's occurredAt and id, appending what arrives", async () => {
    const baseline = logRows(2);
    const newest = baseline[1]!;
    const arrived = logRow({ id: "l-new", occurredAt: new Date(Date.now()).toISOString(), message: "fresh" });
    const connection = fakeConnection({
      tail: (call, nth) => (nth === 1 ? baseline : call.includes("afterId") ? [arrived] : []),
    });
    h.connection = connection;
    render(withSession(<AppLogsSection app="fleet" />));
    await flush();

    expect(connection.callsNamed("logsTail")).toEqual(['builtin logsTail(apps: ["fleet"])']);
    expect(screen.getAllByRole("row")).toHaveLength(2);

    await elapse(2_000);
    const calls = connection.callsNamed("logsTail");
    expect(calls).toHaveLength(2);
    expect(calls[1]).toBe(
      `builtin logsTail(apps: ["fleet"], afterAt: "${newest.occurredAt as string}", afterId: "${newest.id as string}")`,
    );
    expect(screen.getAllByRole("row")).toHaveLength(3);
    expect(screen.getByText("fresh")).toBeTruthy();

    // The next poll carries the NEW newest cursor.
    await elapse(2_000);
    const later = connection.callsNamed("logsTail");
    expect(later[2]).toContain('afterId: "l-new"');
  });

  it("re-runs the baseline while the store is empty, so the first line ever written is not missed", async () => {
    const connection = fakeConnection({ tail: (_call, nth) => (nth < 3 ? [] : logRows(1)) });
    h.connection = connection;
    render(withSession(<AppLogsSection app="fleet" />));
    await flush();
    expect(screen.getByText("Nothing recorded for this app in the last hour.")).toBeTruthy();
    await elapse(2_000);
    expect(connection.callsNamed("logsTail")[1]).toBe('builtin logsTail(apps: ["fleet"])');
    await elapse(2_000);
    expect(screen.getAllByRole("row")).toHaveLength(1);
  });

  it("re-baselines on a facet change: the call carries the facet and no cursor", async () => {
    const connection = fakeConnection({ tail: (_call, nth) => (nth === 1 ? logRows(2) : []) });
    h.connection = connection;
    render(withSession(<AppLogsSection app="fleet" />));
    await flush();
    await elapse(2_000);
    expect(connection.callsNamed("logsTail")[1]).toContain("afterAt");

    await click(screen.getByRole("button", { name: "Refine logs" }));
    await click(screen.getByRole("radio", { name: "Errors" }));
    await flush();
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe('builtin logsTail(apps: ["fleet"], levels: ["error"])');
  });

  it("keeps polling while paused, counts what arrives, and jumps back on the pill", async () => {
    const arrivals: Row[] = [
      logRow({ id: "l-a", occurredAt: new Date(Date.now()).toISOString() }),
      logRow({ id: "l-b", occurredAt: new Date(Date.now() + 500).toISOString() }),
    ];
    const connection = fakeConnection({
      tail: (call, nth) => (nth === 1 ? logRows(2) : call.includes("afterId") && nth === 2 ? arrivals : []),
    });
    h.connection = connection;
    render(withSession(<AppLogsSection app="fleet" />));
    await flush();

    await click(screen.getByRole("button", { name: "Following -- click to pause" }));
    expect(screen.getByRole("button", { name: /^Paused/ })).toBeTruthy();

    await elapse(2_000);
    expect(screen.getByRole("button", { name: "2 new lines -- Jump to latest" })).toBeTruthy();
    // The lines ARE in the list already; the pill is about where the reader is.
    expect(screen.getAllByRole("row")).toHaveLength(4);

    await click(screen.getByRole("button", { name: "2 new lines -- Jump to latest" }));
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
    expect(screen.getByRole("button", { name: /^Following/ })).toBeTruthy();
  });

  it("renders a failed read in surface and keeps polling", async () => {
    const connection = fakeConnection({ tail: logRows(1), refuse: { logsTail: "logs: admin and above" } });
    h.connection = connection;
    render(withSession(<AppLogsSection app="fleet" />));
    await flush();
    expect(screen.getByRole("alert").textContent).toContain("logs: admin and above");
    await elapse(2_000);
    expect(connection.callsNamed("logsTail").length).toBeGreaterThan(1);
  });
});
