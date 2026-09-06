import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// The connection is a module-level context read and its provider dials a real
// websocket, so the HOOK is replaced rather than the provider mounted. The two
// path exports the module also owns have to be restated.
const h = vi.hoisted(() => {
  const runRow = (over: Record<string, unknown> = {}) => ({
    id: "v1:bench:run:1",
    concept: "v1:bench:run",
    tier: "ci",
    commit: "9e91625",
    corpusFingerprint: "53866faa63b0fd5c",
    scenarioCount: 18,
    verdict: "pass",
    runner: "ubuntu-latest",
    startedAt: "2026-09-06T07:00:00Z",
    ...over,
  });
  const sampleRow = (over: Record<string, unknown> = {}) => ({
    id: "v1:bench:sample:" + Math.random().toString(36).slice(2),
    concept: "v1:bench:sample",
    benchRunId: "v1:bench:run:1",
    family: "durability",
    scenarioId: "durability.a-stopped-run-resumes-with-no-duplicated-effect",
    arm: "platform",
    metric: "durability.duplicatedSideEffects",
    unit: "count",
    n: 3,
    median: 0,
    p10: 0,
    p90: 0,
    minimum: 0,
    maximum: 0,
    mad: 0,
    absentReason: "",
    detail: "",
    tier: "ci",
    commit: "9e91625",
    measuredOn: "2026-09-06",
    ...over,
  });

  const samples = [
    sampleRow({ arm: "platform", median: 0 }),
    sampleRow({ arm: "baseline", median: 1 }),
    sampleRow({
      family: "speed",
      metric: "speed.wallClockPerGoalMs",
      unit: "ms",
      arm: "platform",
      absentReason: "notMeasurableOnReplay",
      median: 0,
      n: 0,
    }),
    sampleRow({
      family: "governance",
      metric: "governance.modelCallsJournaled",
      unit: "ratio",
      arm: "platform",
      absentReason: "seamNotBuilt",
      detail: "nothing writes v1:work:modelCall",
      median: 0,
      n: 0,
    }),
  ];

  const connection = {
    nodeId: "bff-test",
    query: {
      benchRuns: vi.fn(async () => ({ rows: () => [runRow()] })),
      benchSamplesForRun: vi.fn(async () => ({ rows: () => samples })),
    },
    subscriptions: null,
    onStatusChange: () => () => {},
  };
  return { connection, samples };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The live collection opens a real subscription from retain(); the feed here
// is not what is under test, so it is replaced with a seeded snapshot. What IS
// under test is what the surface does with the rows.
vi.mock("../../src/live/useLiveCollection", async () => {
  const runRow = {
    id: "v1:bench:run:1",
    concept: "v1:bench:run",
    tier: "ci",
    commit: "9e91625",
    corpusFingerprint: "53866faa63b0fd5c",
    scenarioCount: 18,
    verdict: "pass",
    runner: "ubuntu-latest",
    startedAt: "2026-09-06T07:00:00Z",
  };
  return {
    useLiveCollection: () => ({
      snapshot: { rows: [runRow], state: "live", wasLive: true },
      collection: null,
    }),
  };
});

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { hiddenSurfaces } = await import("../../src/apps/settings/hiddenSurfaces");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role = "owner") {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: role },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function renderBenchmarks(role = "owner") {
  const view = render(wrap(<SettingsApp sectionId="benchmarks" navigate={vi.fn()} askContext={vi.fn()} />, role));
  await screen.findByText("durability.duplicatedSideEffects");
  return view;
}

describe("Benchmarks", () => {
  it("shows the run's provenance, because a number nobody can reproduce is one nobody should act on", async () => {
    await renderBenchmarks();
    expect(screen.getByText("9e91625")).toBeTruthy();
    expect(screen.getByText("53866faa63b0fd5c")).toBeTruthy();
    expect(screen.getByText("18")).toBeTruthy();
  });

  it("renders a measured zero as the number, with its N", async () => {
    await renderBenchmarks();
    const metric = screen.getByText("durability.duplicatedSideEffects").closest("li") as HTMLElement;
    const platform = within(metric).getByText("Platform").closest(".os-bench-arm") as HTMLElement;
    expect(within(platform).getByText("0")).toBeTruthy();
    expect(within(platform).getByText(/N=3/)).toBeTruthy();
  });

  it("renders an unmeasured figure as its REASON and never as a zero", async () => {
    // The assertion the whole surface exists for. A page that showed 0ms here
    // would be claiming a measurement nobody made.
    await renderBenchmarks();
    const metric = screen.getByText("speed.wallClockPerGoalMs").closest("li") as HTMLElement;
    // In the VALUE slot -- the place the number would have been -- not only in
    // the screen-reader summary. That is the design idea: an absence takes the
    // same room as a figure.
    const slot = metric.querySelector(".os-bench-absent")!;
    expect(slot.textContent).toMatch(/Not measurable on a replay/);
    expect(within(metric).queryByText("0ms")).toBeNull();
    expect(metric.querySelector(".os-bench-number")).toBeNull();
  });

  it("names the missing code when a seam is not built", async () => {
    await renderBenchmarks();
    const metric = screen.getByText("governance.modelCallsJournaled").closest("li") as HTMLElement;
    expect(metric.querySelector(".os-bench-absent")!.textContent).toMatch(/nothing writes v1:work:modelCall/);
  });

  it("marks an unmeasured run differently from a measured one, in the picture too", async () => {
    // A bar of height zero is a measurement of zero. The unmeasured mark has
    // to be a different KIND of mark, not a shorter one, or the picture says
    // the opposite of the words beside it.
    const { container } = await renderBenchmarks();
    const measured = container.querySelectorAll('[data-os-bench-mark="measured"]');
    const absent = container.querySelectorAll('[data-os-bench-mark="absent"]');
    expect(measured.length).toBeGreaterThan(0);
    expect(absent.length).toBeGreaterThan(0);
  });

  it("states the platform-against-baseline comparison in words", async () => {
    await renderBenchmarks();
    expect(screen.getByText(/0 on the platform, 1 on the bare loop/)).toBeTruthy();
  });

  it("gives every rail an accessible name and a prose summary", async () => {
    // A chart that could only be read by looking at it would leave its values
    // reachable through hover alone, which is the one thing a chart may not do.
    const { container } = await renderBenchmarks();
    const rails = container.querySelectorAll('[role="img"].os-bench-rail');
    expect(rails.length).toBeGreaterThan(0);
    for (const rail of rails) {
      expect(rail.getAttribute("aria-label")).toMatch(/runs: \d+ measured, \d+ unmeasured/);
    }
    expect(container.querySelectorAll(".os-sr-only").length).toBeGreaterThan(0);
  });

  it("never paints a data series in the warn or error colour", async () => {
    // Amber is warn and red is error everywhere in this shell. A regression is
    // said in words rather than painted.
    const { container } = await renderBenchmarks();
    const html = container.innerHTML;
    expect(html).not.toContain("--os-warn");
    expect(html).not.toContain("--os-error");
  });

  it("is hidden from a reader, and says so in Diagnostics rather than vanishing", () => {
    const hidden = hiddenSurfaces(OS_REGISTRY, "reader");
    expect(hidden.some((s) => s.label.includes("Benchmarks"))).toBe(true);
  });

  it("is reachable by an admin, because the rows are cluster-owner tier", () => {
    const hidden = hiddenSurfaces(OS_REGISTRY, "admin");
    expect(hidden.some((s) => s.label.includes("Benchmarks"))).toBe(false);
  });
});
