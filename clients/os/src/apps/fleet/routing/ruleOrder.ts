// Reordering custom rules, as the writes it takes.
//
// ===========================================================================
// ORDER IS PRECEDENCE, AND TWO RULES MAY NEVER SHARE ONE
// ===========================================================================
// Custom rules are evaluated by precedence, highest first, and the engine
// REFUSES a configuration in which two custom rules hold the same number
// (rule_registry.go: "a tie is resolved by nothing"). It checks the whole
// configuration on every save, so a reorder is a sequence of saves in which
// EVERY intermediate state must be tie-free, not just the last one.
//
// So a move is, in order of preference:
//
//   ONE WRITE    the moved rule takes a number strictly between its new
//                neighbours (or 10 past the end it moved to). Nothing else
//                changes, so nothing else can collide.
//   RENUMBER     when there is no integer between the neighbours, every custom
//                rule is renumbered into a FRESH range that no current value
//                sits in -- the LOWEST such range that fits: below the
//                current minimum when there is room there, otherwise above
//                the current maximum. Each written value is then outside
//                every value still unwritten, so no intermediate state ties.
//                Preferring the low range keeps repeated renumbers from
//                ratcheting upward, and every value stays within the engine's
//                0..1,000,000; when neither range fits even one apart, there
//                is no plan (an empty one) rather than a write the engine
//                would refuse.

export interface Ranked {
  name: string;
  precedence: number;
}

export interface PrecedenceChange {
  name: string;
  precedence: number;
}

/** The engine's ceiling on a precedence. */
export const MAX_PRECEDENCE = 1_000_000;
const STEP = 10;

/**
 * The writes that move `rules[from]` to position `to`.
 *
 * `rules` is the custom tier in evaluation order (precedence descending).
 */
export function reorderPlan(rules: readonly Ranked[], from: number, to: number): PrecedenceChange[] {
  if (from === to || from < 0 || to < 0 || from >= rules.length || to >= rules.length) return [];
  const order = [...rules];
  const [moved] = order.splice(from, 1);
  order.splice(to, 0, moved!);

  const above = order[to - 1];
  const below = order[to + 1];
  let target: number | null;
  if (above && below) {
    const mid = Math.floor((above.precedence + below.precedence) / 2);
    target = mid > below.precedence && mid < above.precedence ? mid : null;
  } else if (below) {
    target = below.precedence + STEP <= MAX_PRECEDENCE ? below.precedence + STEP : null;
  } else if (above) {
    target = above.precedence - STEP >= 0 ? above.precedence - STEP : above.precedence > 0 ? Math.floor(above.precedence / 2) : null;
  } else {
    target = null;
  }
  if (target !== null && target !== moved!.precedence) return [{ name: moved!.name, precedence: target }];
  if (target !== null) return [];

  const max = Math.max(...rules.map((r) => r.precedence));
  const min = Math.min(...rules.map((r) => r.precedence));
  const n = order.length;
  for (const step of [STEP, 1]) {
    // Below: n values, top one strictly under the current minimum.
    if (step * (n - 1) < min) return order.map((rule, i) => ({ name: rule.name, precedence: step * (n - 1 - i) }));
    // Above: n values, bottom one strictly over the current maximum.
    if (max + step * n <= MAX_PRECEDENCE) return order.map((rule, i) => ({ name: rule.name, precedence: max + step * (n - i) }));
  }
  return [];
}
