import { beforeAll, expect, it, vi } from "vitest";
import { webcrypto } from "node:crypto";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { evaluation, measurement, readHistory } from "../../src/apps/training/evaluations";
import { evaluationFixture } from "./evaluationFixture";
import { EvaluationsSection } from "../../src/apps/training/EvaluationsSection";

const state = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => state.connection }));
beforeAll(() => { Object.defineProperty(globalThis, "crypto", { value: webcrypto, configurable: true }); });
const rows = (data: Record<string, unknown>[], hasMore = false, cursor?: string) => new Result({ data, meta: { hasMore, cursor } });

it("rejects a forged green outcome, modified routes and incomplete model records", async () => {
  const good = await evaluationFixture();
  expect(await evaluation(good)).not.toBeNull();
  for (const bad of [
    { ...good, outcome: "autonomous" },
    { ...good, assertions: [] },
    { ...good, modelsUsed: [] },
    { ...good, setup: { ...good.setup, routes: [] } },
    { ...good, setup: { ...good.setup, routingDigest: "forged" } },
    { ...good, modelsUsed: [{}] },
    { ...good, modelsUsed: [{ ...good.modelsUsed[0], source: "federation" }] },
    { ...good, bindings: [{ ...good.bindings[0], digest: "not-a-digest" }] },
    { ...good, interventions: [{ ...good.interventions[0], at: "2030-01-01T00:00:00Z" }] },
    { ...good, outcome: "failed", artifacts: [] },
  ]) expect(await evaluation(bad)).toBeNull();
  expect(measurement(good.inputTokens)).toBe("Not measured");
  expect(measurement({ basis: "estimated", value: 0, source: "visible text" })).toBe("0 (estimated)");
});

it("reads subsequent result pages and refuses a repeated cursor", async () => {
  const read = vi.fn().mockResolvedValueOnce(rows([{ id: "one" }], true, "page2")).mockResolvedValueOnce(rows([{ id: "two" }]));
  expect(await readHistory(read)).toEqual([{ id: "one" }, { id: "two" }]);
  expect(read).toHaveBeenLastCalledWith("page2");
  await expect(readHistory(async () => rows([], true, "same"))).rejects.toThrow("incomplete page");
});

it("shows the whole setup, actual models, intervention and unmeasured usage", async () => {
  const result = await evaluationFixture();
  state.connection = { query: { workGoalEvaluationsForOwner: vi.fn().mockResolvedValue(rows([{ data: { goalEvaluation: result } }])) } };
  render(<EvaluationsSection />);
  expect(await screen.findByText("MemQL · Synthetic QA setup")).toBeTruthy();
  expect(screen.getAllByText("Coming soon")).toHaveLength(2);
  fireEvent.click(screen.getByText("Investigate and reproduce"));
  expect(await screen.findByText("Models actually used")).toBeTruthy();
  expect(screen.getByText("Synthetic guidance for the inspection test")).toBeTruthy();
  expect(screen.getAllByText("fixture-reasoning-model", { exact: false }).length).toBeGreaterThan(0);
  expect(screen.getAllByText("Not measured").length).toBeGreaterThan(0);
  cleanup();
});
