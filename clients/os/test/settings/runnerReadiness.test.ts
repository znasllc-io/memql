import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readRunnerReport, runnerWords, type RunnerReading } from "../../src/apps/settings/runnerReadiness";

const runner: RunnerReading = { nodeId: "workbench-a", available: true, isolation: "passed", validUntil: "2026-10-05T20:01:00Z", detail: "" };
const report = { checkedAt: "2026-10-05T20:00:00Z", runners: [runner] };

describe("runner observations", () => {
  it("reads the builtin envelope without treating a missing report as an empty fleet", () => {
    const rows = [{ integrationStatus: { checkedAt: report.checkedAt, integrations: [{ name: "pipelines", runners: [runner] }] } }] as Row[];
    expect(readRunnerReport(rows)).toEqual(report);
    expect(readRunnerReport([{ integrations: [{ name: "pipelines" }] }] as Row[])).toBeNull();
    expect(readRunnerReport([{ integrations: [{ name: "pipelines", runners: [] }] }] as Row[])?.runners).toEqual([]);
  });

  it("requires a live runner and a current, dated proof", () => {
    expect(runnerWords(runner, report, 0)).toBe("Ready");
    expect(runnerWords({ ...runner, available: false }, report, 0)).toBe("Unavailable");
    expect(runnerWords({ ...runner, isolation: "not_proven" }, report, 0)).toBe("Isolation not checked");
    expect(runnerWords({ ...runner, isolation: "inconclusive" }, report, 0)).toBe("Isolation not confirmed");
    expect(runnerWords({ ...runner, isolation: "unexpected" }, report, 0)).toBe("Readiness unknown");
    expect(runnerWords({ ...runner, validUntil: "" }, report, 0)).toBe("Readiness unknown");
    expect(runnerWords({ ...runner, validUntil: "2026-10-05T20:00:01Z" }, report, 1_001)).toBe("Isolation check expired");
  });

  it("ages out an observation even when the isolation proof lasts longer", () => {
    expect(runnerWords({ ...runner, validUntil: "2026-10-05T21:00:00Z" }, report, 60_001)).toBe("Readiness needs refresh");
  });
});
