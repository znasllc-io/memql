// Labels, in the two shapes the Fleet needs them: the MAP the graph stores
// and the `key=value` CHIP a person edits.
//
// ===========================================================================
// TWO LABEL FIELDS, AND THE SPLIT IS THE WHOLE POINT
// ===========================================================================
// A machine carries `labels` -- what the cockpit reports -- and
// `operatorLabels` -- what its owner set from a Fleet surface. The reported
// map is OVERWRITTEN WHOLESALE from the Register message on every reconnect
// (component/worker/server.go's upsertRegistration), so an operator tag
// written into it would survive until the machine's next restart and then
// vanish. That is the worst failure mode a routing input has: the rule still
// reads correctly and the machine silently stops matching it.
//
// The router matches on the MERGE of the two with the operator side winning,
// so any surface an operator reads to understand routing has to show the
// merge -- and has to say WHICH SIDE a value came from, because a reported
// value an operator has overridden is a fact about their configuration rather
// than about the machine.
//
// This restates the rule the portal's fleet/labels.ts carried until epic
// memql#4984 retired it. The two
// are deliberately separate files: the portal's is coupled to its LabelChips
// primitive and its `rowObject` helper, and the OS shares no UI kit with it.
// What must NOT diverge is the RULE -- operator over reported, `overrides`
// meaning "replaced a reported value" -- which is why it is written down here
// rather than inlined at a render site.

/** A label map as it sits on a row: string keys, string values. */
export type LabelMap = Record<string, string>;

/** Which side of the merge a surviving value came from. */
export type LabelSource = "reported" | "operator";

export interface MergedLabel {
  key: string;
  value: string;
  source: LabelSource;
  /**
   * True when the operator's value REPLACED a reported one for the same key.
   * Distinct from `source === "operator"` -- an operator-only key overrides
   * nothing -- and worth its own flag because it is the case where the
   * machine tells the cluster one thing and the routing acts on another.
   */
  overrides: boolean;
}

/**
 * Read one object-typed field off a row payload as a label map.
 *
 * Non-string values are COERCED for numbers and booleans and DROPPED for
 * everything else. `[object Object]` in a routing predicate is worse than an
 * absent key, because it looks like a value somebody chose.
 */
export function labelMapFrom(raw: unknown): LabelMap {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return {};
  const out: LabelMap = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === "string") out[key] = value;
    else if (typeof value === "number" || typeof value === "boolean") out[key] = String(value);
  }
  return out;
}

/**
 * Fold the operator's map over the reported one, operator side winning, and
 * report where every surviving value came from. Sorted by key so the same
 * machine reads the same way on every render.
 */
export function mergeLabels(reported: LabelMap, operator: LabelMap): MergedLabel[] {
  const keys = new Set([...Object.keys(reported), ...Object.keys(operator)]);
  const out: MergedLabel[] = [];
  for (const key of [...keys].sort()) {
    const operatorValue = operator[key];
    if (operatorValue !== undefined) {
      out.push({
        key,
        value: operatorValue,
        source: "operator",
        overrides: reported[key] !== undefined,
      });
      continue;
    }
    out.push({ key, value: reported[key] ?? "", source: "reported", overrides: false });
  }
  return out;
}

// ---------------------------------------------------------------------------
// The chip form
// ---------------------------------------------------------------------------
//
// A worker label is a PAIR, and the editor works in flat strings, so the chip
// text is the pair written the way an operator would type it: `key=value`.
// That is also the form docs/public/operate/workers-runbook.md's worker.yaml
// already shows, so there is one spelling to learn rather than two.

/** The separator. One export, because the parser, the formatter, the
 *  placeholder copy and the tests all have to agree on it. */
export const CHIP_SEPARATOR = "=";

/**
 * Split `key=value` into its halves, or return null when the text is not a
 * pair.
 *
 * Splits on the FIRST separator only: a value may legitimately contain one
 * ("path=/opt/a=b"), a key may not, and there is no escaping to invent. A
 * blank key is refused -- `=x` names nothing -- and so is a chip with no
 * separator, because a bare word is a value with no key rather than a key
 * with no value, and guessing which the operator meant is how a routing rule
 * ends up matching something nobody wrote.
 */
export function parseLabelChip(text: string): { key: string; value: string } | null {
  const at = text.indexOf(CHIP_SEPARATOR);
  if (at < 0) return null;
  const key = text.slice(0, at).trim();
  const value = text.slice(at + CHIP_SEPARATOR.length).trim();
  if (key === "") return null;
  return { key, value };
}

/** parseLabelChip's inverse. */
export function formatLabelChip(key: string, value: string): string {
  return `${key}${CHIP_SEPARATOR}${value}`;
}

/** Render a map as the sorted chip list an editor takes. */
export function chipsFromMap(map: LabelMap): string[] {
  return Object.keys(map)
    .sort()
    .map((key) => formatLabelChip(key, map[key] ?? ""));
}

/** chipsFromMap's inverse; unparseable chips are dropped. */
export function mapFromChips(chips: readonly string[]): LabelMap {
  const out: LabelMap = {};
  for (const chip of chips) {
    const pair = parseLabelChip(chip);
    if (pair === null) continue;
    out[pair.key] = pair.value;
  }
  return out;
}
