import type { SynapseField, SynapsePatch } from "./types";

// Turning a model's reply into patches a form can take.
//
// ===========================================================================
// THE THIRD OF THREE DEFENCES
// ===========================================================================
// The engine's template forbids inventing a field; the engine's handler drops
// any patch outside the declared scope before serializing; and this drops
// them again on the way in. Three checks is not paranoia about the model -- it
// is that a patch lands in a form somebody is about to submit, and the client
// is the only party that knows what its own field types mean.
//
// DROP, NEVER GUESS. A value that will not coerce is discarded rather than
// approximated: "about five" is not 5, and a form silently holding a number
// nobody chose is worse than a field left empty. Every drop is silent to the
// UI and deliberate -- the person can see what was filled and what was not,
// which is the honest report.

export function coercePatches(
  raw: unknown,
  fields: readonly SynapseField[],
): SynapsePatch[] {
  const byName = new Map(fields.map((field) => [field.name, field]));
  const out: SynapsePatch[] = [];
  if (!Array.isArray(raw)) return out;

  for (const item of raw) {
    if (item === null || typeof item !== "object") continue;
    const record = item as Record<string, unknown>;
    const name = typeof record["field"] === "string" ? record["field"].trim() : "";
    const field = byName.get(name);
    if (field === undefined) continue;

    const value = coerce(field, record["value"]);
    if (value === undefined) continue;
    // First one wins. A reply naming the same field twice is a reply that
    // disagrees with itself, and taking the LAST would mean the value a
    // person sees depends on ordering nobody specified.
    if (out.some((patch) => patch.field === name)) continue;
    out.push({ field: name, value });
  }
  return out;
}

function coerce(field: SynapseField, raw: unknown): string | number | boolean | undefined {
  const text = typeof raw === "string" ? raw.trim() : raw === undefined || raw === null ? "" : String(raw);
  if (text === "") return undefined;

  switch (field.type) {
    case "number": {
      // Number("") is 0 and Number(" 12 ") is 12, so the empty case is
      // already out above and the trim is already done.
      const n = Number(text);
      return Number.isFinite(n) ? n : undefined;
    }
    case "boolean": {
      const lower = text.toLowerCase();
      if (["true", "yes", "on", "1"].includes(lower)) return true;
      if (["false", "no", "off", "0"].includes(lower)) return false;
      // Anything else is a sentence about a checkbox, not a checkbox.
      return undefined;
    }
    case "enum": {
      const options = field.options ?? [];
      // Case-insensitive, because a person says "macos" and the option is
      // "macOS" -- but the value written is the OPTION's own spelling, never
      // the person's, so the form receives something it can store.
      const hit = options.find((option) => option.toLowerCase() === text.toLowerCase());
      return hit;
    }
    default:
      return text;
  }
}
