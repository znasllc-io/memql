import { catalog, fingerprint, type Evaluation } from "../../src/apps/training/evaluations";

// Synthetic presentation fixture. Never a measured baseline.
export async function evaluationFixture(): Promise<Evaluation> {
  const s = catalog.scenarios[0]!;
  const unknown = { basis: "unavailable" as const, source: "Synthetic QA fixture has no provider measurement" };
  const routes = ["reasoning", "planning", "validation", "embeddings"].map((role) => ({ role, level: role === "embeddings" ? "embeddings" : "strong", rule: `fixture-${role}`, doors: [{ source: "fleet", model: `fixture-${role}-model`, configuration: {} }] }));
  return {
    schemaVersion: 1, id: "qa-one", scenarioId: s.id, scenarioVersion: s.version, scenarioFingerprint: await fingerprint(s), fixtureVersion: s.fixture.version,
    engineCommit: "e".repeat(40), harnessCommit: "f".repeat(40), goalId: "fixture-goal", runId: "fixture-run",
    setup: { name: "Synthetic QA setup", routingDigest: await fingerprint({ routes, paidProvidersAllowed: false }), routes, paidProvidersAllowed: false },
    modelsUsed: routes.map((r) => ({ role: r.role, level: r.level, source: "fleet", model: r.doors[0]!.model, calls: 2, fallback: false, evidence: ["fixture:call-receipt"], inputTokens: unknown, outputTokens: unknown, costUsd: unknown })),
    outcome: "assisted", assertions: s.criteria.map((c) => ({ criterionId: c.id, verdict: "pass", evidence: ["fixture:oracle-evidence"] })),
    interventions: [{ at: "2026-10-13T00:01:00Z", kind: "guidance", description: "Synthetic guidance for the inspection test", evidence: "fixture:guidance" }],
    bindings: [{ id: "software-development", version: "1", digest: "a".repeat(64), source: "memql-baseline" }],
    artifacts: ["fixture:test-output"], startedAt: "2026-10-13T00:00:00Z", finishedAt: "2026-10-13T00:02:00Z",
    inputTokens: unknown, outputTokens: unknown, costUsd: unknown,
  };
}
