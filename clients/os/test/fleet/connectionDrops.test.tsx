import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

// A machine's Details > Connection shows its recent connection drops
// (network-aware fleet selection): the rows the cluster records when the
// machine's connection ended in a way Cockpit did not choose. A count is a
// status, so it is read and not copied (DESIGN.md, "Quiet copying"); a read
// that failed says so rather than showing an invented zero.

const { connectionDropsFromRows, connectionDropsWords } = await import("../../src/apps/fleet/machines/connectionDrops");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MachineDetailsInfo } = await import("../../src/apps/fleet/machines/MachineDetailsInfo");
const { machineRow } = await import("./harness");
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
