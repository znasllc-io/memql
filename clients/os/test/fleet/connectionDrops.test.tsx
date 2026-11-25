import { cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

// A machine's Details > Connection shows its recent connection drops
// (network-aware fleet selection): the rows the cluster records when the
// machine's connection ended in a way Cockpit did not choose. A count is a
// status, so it is read and not copied (DESIGN.md, "Quiet copying"); a read
// that failed says so rather than showing an invented zero.

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read whose provider dials a real
// socket; replacing the READ lets the real reading path run under jsdom, the
// seam every other test in this directory uses.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { connectionDropsFromRows, connectionDropsWords, useConnectionDrops } = await import("../../src/apps/fleet/machines/connectionDrops");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MachineDetailsInfo } = await import("../../src/apps/fleet/machines/MachineDetailsInfo");
const { fakeConnection, machineRow, rowsResult, withSession } = await import("./harness");
const { formatFreshness } = await import("../../src/kit/format");

afterEach(() => cleanup());

const NOW = new Date("2026-11-24T20:00:00Z");
const drop = (at: string) => ({ id: `drop-${at}`, droppedAt: at, cause: "replaced" });

describe("connection drops words", () => {
  it("says none, one, several and capped counts, with the last one's age", () => {
    expect(connectionDropsWords(connectionDropsFromRows([]), NOW)).toEqual({ value: "None in the last day" });
    const last = "2026-11-24T18:06:26Z";
    expect(connectionDropsWords(connectionDropsFromRows([drop(last)]), NOW)).toEqual({
      value: `1 in the last day, last ${formatFreshness(last, NOW)}`, title: last,
    });
    const three = connectionDropsFromRows([drop("2026-11-24T19:44:56Z"), drop("2026-11-24T19:02:31Z"), drop(last)]);
    expect(connectionDropsWords(three, NOW).value).toBe(`3 in the last day, last ${formatFreshness("2026-11-24T19:44:56Z", NOW)}`);
    const hundred = connectionDropsFromRows(Array.from({ length: 100 }, (_, i) => drop(new Date(NOW.getTime() - i * 60_000).toISOString())));
    expect(connectionDropsWords(hundred, NOW).value.startsWith("100 or more in the last day, last ")).toBe(true);
  });

  it("never shows a figure it did not read", () => {
    expect(connectionDropsWords({ state: "reading" }, NOW)).toEqual({ value: "Checking" });
    expect(connectionDropsWords({ state: "unavailable" }, NOW)).toEqual({ value: "Could not be read" });
  });
});

describe("the Connection section", () => {
  it("shows the drops as a status without a copy icon", () => {
    const machine = machineFromRow(machineRow({ id: "v1:worker:registration:a", displayName: "Alpha", name: "alpha.local", lastSeenAt: "2026-11-24T19:59:55Z" }));
    render(<MachineDetailsInfo machine={machine} now={NOW} machineClass="" usableBytes={0}
      drops={connectionDropsFromRows([drop("2026-11-24T19:44:56Z")])} />);
    expect(screen.getByText(`1 in the last day, last ${formatFreshness("2026-11-24T19:44:56Z", NOW)}`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Copy Connection drops" })).toBeNull();
  });
});

describe("one machine's drops", () => {
  const session = ({ children }: { children: ReactNode }) => withSession(children, { userId: "me" });

  it("asks for the machine it names over the last day, in the width the rows are written in", async () => {
    const asked: Record<string, unknown>[] = [];
    const last = "2026-11-24T19:44:56.123456789Z";
    h.connection = fakeConnection({}, {
      myWorkerConnectionDrops: async (args) => {
        asked.push(args);
        return rowsResult([drop(last), drop("2026-11-24T18:06:26.000000000Z")]);
      },
    });
    const before = Date.now();
    const view = renderHook(() => useConnectionDrops("reg-a", true), { wrapper: session });
    await waitFor(() => expect(view.result.current.drops.state).toBe("read"));
    expect(asked).toHaveLength(1);
    const [first] = asked;
    expect(first?.registrationId).toBe("reg-a");
    // The window is compared as text, so its start has the rows' nine
    // fraction digits (component/worker DropTimeLayout), not toISOString's three.
    const since = String(first?.since);
    expect(since).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$/);
    const lookback = before - Date.parse(since);
    expect(lookback).toBeGreaterThan(24 * 60 * 60 * 1000 - 5_000);
    expect(lookback).toBeLessThan(24 * 60 * 60 * 1000 + 5_000);
    expect(view.result.current.drops).toEqual({ state: "read", count: 2, capped: false, lastAt: last });
  });

  it("says it could not read rather than showing an invented zero", async () => {
    h.connection = fakeConnection({}, {
      myWorkerConnectionDrops: async () => {
        throw new Error("stream closed");
      },
    });
    const view = renderHook(() => useConnectionDrops("reg-a", true), { wrapper: session });
    await waitFor(() => expect(view.result.current.drops.state).toBe("unavailable"));
  });

  it("reads nothing while its section is closed", async () => {
    const asked: unknown[] = [];
    h.connection = fakeConnection({}, {
      myWorkerConnectionDrops: async (args) => {
        asked.push(args);
        return rowsResult([]);
      },
    });
    const view = renderHook(() => useConnectionDrops("reg-a", false), { wrapper: session });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(asked).toHaveLength(0);
    expect(view.result.current.drops).toEqual({ state: "reading" });
  });
});
