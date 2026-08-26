import { rowObject, type Row } from "@znasllc-io/memql-sdk-core/client";

// Labels, in the two shapes this surface needs them: the MAP the graph stores
// and the `key=value` CHIP the person edits.
//
// ===========================================================================
// WHY TWO LABEL FIELDS, AND WHY THE MERGE IS NOT A DETAIL
// ===========================================================================
// A machine carries `labels` -- what the cockpit reports -- and
// `operatorLabels` -- what its owner set from this page. The split exists
// because the reported map is OVERWRITTEN WHOLESALE from the Register message
// on every reconnect: an operator tag written into `labels` would survive
// until the machine's next restart and then vanish, which is the worst
// possible failure mode for a routing input (the rule still reads correctly,
// the machine silently stops matching it).
//
// The router matches on the MERGE of the two with the operator side winning,
// so this page has to show the merge -- a table listing only `labels` would
// disagree with the routing an operator is looking at it to understand. It
// also has to show WHICH SIDE a value came from, because a reported value that
// an operator has overridden is a fact about their own configuration, not
// about the machine.

// A label map as it sits on a row: string keys, string values. Non-string
// values are dropped rather than stringified -- `[object Object]` in a routing
// predicate is worse than an absent key, because it looks like a value.
export type LabelMap = Record<string, string>;

// Which side of the merge a value came from.
export type LabelSource = "reported" | "operator";

export interface MergedLabel {
  key: string;
  value: string;
  source: LabelSource;
  // True when the operator's value REPLACED a reported one for the same key.
  // Distinct from source === "operator" (an operator-only key overrides
  // nothing), and worth its own flag because it is the case where the machine
  // is telling the cluster one thing and the routing acts on another.
  overrides: boolean;
}

// labelMapFromRow reads one object-typed field off a row as a label map.
export function labelMapFromRow(row: Row | null, field: string): LabelMap {
  const raw = rowObject(row, field);
  if (raw === null) return {};
  const out: LabelMap = {};
  for (const [key, value] of Object.entries(raw)) {
    if (typeof value === "string") out[key] = value;
    else if (typeof value === "number" || typeof value === "boolean") out[key] = String(value);
  }
  return out;
}

// mergeLabels folds the operator's map over the reported one, operator side
// winning, and reports where every surviving value came from. Sorted by key so
// the same machine reads the same way on every render.
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
// LabelChips (src/ui/LabelChips.tsx) is the portal's one label editor and it
// works in FLAT STRINGS -- it is the primitive the Artifacts page uses for a
// []string field. A worker label is a PAIR, so the chip text is the pair
// written the way an operator would type it: `key=value`.
//
// The alternative was a second editor taking two inputs. It is not better: the
// portal would then have two label controls that look different and behave
// differently, and the one thing an operator has to learn here (Enter commits)
// would have to be learned twice. `key=value` is also the form the workers
// runbook's worker.yaml already shows.

// The separator. A single export because the parser, the formatter, the
// placeholder copy and the tests all have to agree on it.
export const CHIP_SEPARATOR = "=";

// parseLabelChip splits `key=value` into its halves, or returns null when the
// text is not a pair.
//
// Splits on the FIRST separator only: a value may legitimately contain one
// ("path=/opt/a=b"), a key may not, and there is no escaping to invent.
// A blank key is refused -- `=x` names nothing -- and so is a chip with no
// separator, because a bare word is a value with no key rather than a key with
// no value, and guessing which the operator meant is how a routing rule ends
// up matching something nobody wrote.
export function parseLabelChip(text: string): { key: string; value: string } | null {
  const at = text.indexOf(CHIP_SEPARATOR);
  if (at < 0) return null;
  const key = text.slice(0, at).trim();
  const value = text.slice(at + CHIP_SEPARATOR.length).trim();
  if (key === "") return null;
  return { key, value };
}

// formatLabelChip is parseLabelChip's inverse.
export function formatLabelChip(key: string, value: string): string {
  return `${key}${CHIP_SEPARATOR}${value}`;
}

// chipsFromMap renders a map as the sorted chip list LabelChips takes.
export function chipsFromMap(map: LabelMap): string[] {
  return Object.keys(map)
    .sort()
    .map((key) => formatLabelChip(key, map[key] ?? ""));
}

// mapFromChips is chipsFromMap's inverse; unparseable chips are dropped.
export function mapFromChips(chips: readonly string[]): LabelMap {
  const out: LabelMap = {};
  for (const chip of chips) {
    const pair = parseLabelChip(chip);
    if (pair === null) continue;
    out[pair.key] = pair.value;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Model advertisement labels (epic memql#4676)
// ---------------------------------------------------------------------------
//
// A machine advertises what it can serve as `model:<modelId>` and
// `runtime:<name>` in its REPORTED labels. Both helpers below read only that
// half, deliberately: an operator-set `model:` label would be a claim the
// machine never made, and the router -- which matches on what the machine
// reports -- would refuse the call it implied.
//
// The model id itself contains a colon (`llama3.1:8b`), which is why these
// parse by PREFIX rather than splitting on the separator. A naive split eats
// the tag and leaves "llama3.1", which is not a model anything hosts.

const MODEL_PREFIX = "model:";
const RUNTIME_PREFIX = "runtime:";

export function modelsFromLabels(reported: LabelMap): string[] {
  const out: string[] = [];
  for (const key of Object.keys(reported ?? {})) {
    if (!key.startsWith(MODEL_PREFIX)) continue;
    const id = key.slice(MODEL_PREFIX.length).trim();
    if (id !== "") out.push(id);
  }
  return out.sort();
}

export function runtimesFromLabels(reported: LabelMap): string[] {
  const out: string[] = [];
  for (const key of Object.keys(reported ?? {})) {
    if (!key.startsWith(RUNTIME_PREFIX)) continue;
    const name = key.slice(RUNTIME_PREFIX.length).trim();
    if (name !== "") out.push(name);
  }
  return out.sort();
}
