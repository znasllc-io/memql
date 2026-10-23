import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { formatTokens, sameRow } from "./rows";
import { flatten } from "../../kit/rows";

export interface ModelUsage {
  id: string; runId: string; stepKey: string; model: string;
  input: number | null; output: number | null; estimated: boolean;
  cost: number | null; billing: string; cacheKind: string;
}
export interface UsageSummary {
  calls: number; input: number | null; output: number | null; total: number | null;
  estimated: boolean; incomplete: boolean;
}
const text = (value: unknown) => typeof value === "string" ? value : "";
const count = (value: unknown): number | null => typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : null;

export function modelUsageFromRows(rows: readonly Row[], runId: string): ModelUsage[] {
  const unique = new Map<string, ModelUsage>();
  for (const source of rows) {
    const row = flatten(source);
    if (!sameRow(text(row.runId), runId)) continue;
    // DSL shapes name each projection by its terminal field, so call.id is
    // the projected id (not the observation row's private storage identity).
    const call = row;
    const id = text(call.id);
    // A stable recorded call identity prevents repeated progress receipts from
    // becoming extra usage. No model-call placeholder zeros participate.
    if (!id || typeof call.tokensEstimated !== "boolean" || unique.has(id)) continue;
    unique.set(id, { id, runId, stepKey: text(row.stepKey), model: text(call.model),
      input: count(call.inputTokens), output: count(call.outputTokens), estimated: call.tokensEstimated === true,
      cost: call.pricingConfigured === true ? count(call.totalCost) : null,
      billing: text(call.billing), cacheKind: text(call.cacheKind) });
  }
  return [...unique.values()];
}
export function summarizeUsage(calls: readonly ModelUsage[], complete: boolean): UsageSummary {
  const sum = (key: "input" | "output") => calls.length && calls.some(call => call[key] !== null) ? calls.reduce((total, call) => total + (call[key] ?? 0), 0) : null;
  const input = sum("input"), output = sum("output");
  return { calls: calls.length, input, output, total: input === null || output === null ? null : input + output,
    estimated: calls.some(call => call.estimated), incomplete: !complete || calls.some(call => call.input === null || call.output === null) };
}
export function usageLabel(usage: UsageSummary): string {
  if (usage.total === null) return "Usage not reported";
  return `${formatTokens(usage.total)} ${usage.estimated ? "estimated" : "reported"} tokens${usage.incomplete ? " shown" : ""}`;
}
