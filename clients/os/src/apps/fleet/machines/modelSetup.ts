import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../../kit/rows";

export const MODEL_PURPOSES = [
  ["conversation", "Conversation"], ["structured", "Structured output"],
  ["tools", "Tools & agents"], ["vision", "Images & documents"],
  ["embeddings", "Search & memory"], ["audioIn", "Transcription"],
  ["audioOut", "Speech"], ["imageGen", "Image generation"],
] as const;
export type ModelChoices = Record<string, string>;
export interface SetupModel {
  modelId: string; category: string; runtime: string; purposes: string[];
  memoryBytes: number; sizeBytes: number; contextWindow: number;
  installed: boolean; blocked: string; installBlocked: string; recommended: boolean;
  source: string; notes: string; reviewedAt?: string;
}
export interface ModelSetupView {
  boundEmbeddingModel: string; revision: string; choices: ModelChoices; suggestedChoices: ModelChoices; catalog: SetupModel[];
  researchSummary: string; researchAt: string; researchEnabled: boolean;
  usableBytes: number; diskFreeBytes: number; contextLimit: number; warning: string;
}
const object = (v: unknown): Record<string, unknown> => v !== null && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : {};
const str = (v: unknown): string => typeof v === "string" ? v : "";
const number = (v: unknown): number => typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0;
export function modelSetupFromRow(raw: Row): ModelSetupView {
  const row = flatten(raw) as Record<string, unknown>;
  return {
    suggestedChoices: Object.fromEntries(Object.entries(object(row.suggestedChoices)).filter((entry): entry is [string, string] => typeof entry[1] === "string")),
    researchSummary: str(row.researchSummary), researchAt: str(row.researchAt), researchEnabled: row.researchEnabled === true,
    boundEmbeddingModel: str(row.boundEmbeddingModel), revision: str(row.revision), choices: Object.fromEntries(Object.entries(object(row.choices)).filter((entry): entry is [string, string] => typeof entry[1] === "string")),
    usableBytes: number(row.usableBytes), diskFreeBytes: number(row.diskFreeBytes), contextLimit: number(row.contextLimit), warning: str(row.warning),
    catalog: (Array.isArray(row.catalog) ? row.catalog : []).map(value => {
      const m = object(value);
      return { modelId: str(m.modelId), category: str(m.category), runtime: str(m.runtime),
        purposes: Array.isArray(m.purposes) ? m.purposes.filter((p): p is string => typeof p === "string") : [],
        memoryBytes: number(m.memoryBytes), sizeBytes: number(m.sizeBytes), contextWindow: number(m.contextWindow),
        installed: m.installed === true, blocked: str(m.blocked), installBlocked: str(m.installBlocked), recommended: m.recommended === true,
        source: str(m.source), notes: str(m.notes), reviewedAt: str(m.reviewedAt) };
    }),
  };
}
export function setupCapacity(choices: ModelChoices, catalog: SetupModel[], budget: number) {
  const selected = [...new Set(Object.values(choices).filter(Boolean))];
  const models = selected.map(id => catalog.find(m => m.modelId === id));
  const known = models.every(m => m && m.memoryBytes > 0);
  const used = models.reduce((sum, m) => sum + (m?.memoryBytes ?? 0), 0);
  const invalid = Object.entries(choices).some(([purpose, id]) => {
    if (!id) return false;
    const m = catalog.find(p => p.modelId === id);
    return !m || !m.installed || !!m.blocked || !m.purposes.includes(purpose);
  });
  return { selected, used, known, fits: known && !invalid && (selected.length === 0 || budget > 0) && used <= budget };
}
/** The same validation serves clicks, keyboard activation, drops and batch assignments. */
export function assignmentProposal(choices: ModelChoices, modelId: string, purposes: string[], view: ModelSetupView): { choices: ModelChoices; refusal: string } {
  const next = { ...choices };
  const model = view.catalog.find(m => m.modelId === modelId);
  for (const purpose of purposes) {
    if (!MODEL_PURPOSES.some(([id]) => id === purpose)) return { choices, refusal: "That task is unavailable." };
    if (purpose === "embeddings" && view.boundEmbeddingModel && modelId !== view.boundEmbeddingModel) return { choices, refusal: `Memory search keeps ${view.boundEmbeddingModel}. Switching requires a reindex.` };
    if (modelId) {
      if (!model?.installed) return { choices, refusal: "Install this model before assigning it." };
      if (model.blocked) return { choices, refusal: model.blocked };
      if (!model.purposes.includes(purpose)) return { choices, refusal: "This model does not support that task." };
      if (!model.memoryBytes) return { choices, refusal: "This model's memory use has not been verified." };
    }
    next[purpose] = modelId;
  }
  // Clearing an assignment must remain possible when repairing an invalid saved setup.
  if (modelId) {
    const capacity = setupCapacity(next, view.catalog, view.usableBytes);
    if (!capacity.known) return { choices, refusal: "Clear the unavailable assignments first." };
    if (!view.usableBytes || capacity.used > view.usableBytes) return { choices, refusal: "This would exceed capacity. Reuse a model or choose a smaller one." };
  }
  return { choices: next, refusal: "" };
}
export function sameModelChoices(a: ModelChoices, b: ModelChoices) {
  return MODEL_PURPOSES.every(([id]) => (a[id] || "") === (b[id] || ""));
}
export function safeModelSource(source: string): string | undefined {
  try { const url = new URL(source); return url.protocol === "https:" ? url.href : undefined; } catch { return undefined; }
}
