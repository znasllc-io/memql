// What a fire costs, and how the number on the float is arrived at.
//
// ===========================================================================
// WHY SHOW IT AT ALL (decision D8)
// ===========================================================================
// Every Synapse fire spends somebody's money. An affordance that hides that
// teaches a person the button is free, and the first time they learn
// otherwise is a bill. A number that rises off the button for a second is the
// cheapest possible honesty: it is unmissable the first few times and
// invisible once you have internalised the cost, which is exactly the
// attention curve the fact deserves.
//
// ===========================================================================
// THE ESTIMATE, AND WHEN IT IS A GUESS
// ===========================================================================
// Per SCOPE, because "filling the add-machine form" and "composing a view"
// cost different amounts and averaging them together would describe neither.
//
//   * a reply that reports usage UPDATES the average and the float shows the
//     real number.
//   * a reply that does not report usage is estimated from the prompt and the
//     schema at roughly four characters per token, and the float is marked
//     with a tilde -- a guessed number that looked measured would be worse
//     than no number.
//   * a scope with no history at all shows a quiet "first run" cue instead of
//     a figure, because an average over zero samples is not an average.
//
// EVERY READ AND WRITE IS WRAPPED. localStorage throws in a private window,
// under a blocking profile, and inside some embedded views; a spend indicator
// is not worth taking a page down for.

const KEY_PREFIX = "memql-portal-synapse-tokens-";

// A short window. The point is "what does this cost lately", and a lifetime
// average would take a hundred cheap fires to reflect a model change.
const WINDOW = 10;

interface Average {
  readonly n: number;
  readonly avg: number;
}

function keyFor(scopeId: string): string {
  return KEY_PREFIX + scopeId;
}

export function readAverage(scopeId: string): Average | null {
  try {
    const raw = globalThis.localStorage?.getItem(keyFor(scopeId));
    if (raw === null || raw === undefined) return null;
    const parsed: unknown = JSON.parse(raw);
    if (parsed === null || typeof parsed !== "object") return null;
    const record = parsed as Record<string, unknown>;
    const n = typeof record["n"] === "number" ? record["n"] : 0;
    const avg = typeof record["avg"] === "number" ? record["avg"] : 0;
    // A half-written or hand-edited blob reads as "no history", which is the
    // state the UI already handles.
    if (!Number.isFinite(n) || !Number.isFinite(avg) || n <= 0 || avg <= 0) return null;
    return { n, avg };
  } catch {
    return null;
  }
}

export function recordUsage(scopeId: string, tokens: number): void {
  if (!Number.isFinite(tokens) || tokens <= 0) return;
  const previous = readAverage(scopeId);
  // A running mean over the last WINDOW samples: cheap, needs no array, and
  // converges fast enough that the second fire already reflects the first.
  const n = Math.min((previous?.n ?? 0) + 1, WINDOW);
  const avg = previous === null ? tokens : previous.avg + (tokens - previous.avg) / n;
  try {
    globalThis.localStorage?.setItem(keyFor(scopeId), JSON.stringify({ n, avg }));
  } catch {
    // See the header: a spend indicator is not worth a failed render.
  }
}

export interface TokenFloat {
  // Absent on a scope's first fire -- there is nothing honest to put here.
  readonly tokens: number | null;
  // True when the figure is derived from character counts rather than from a
  // reply that reported its own usage. The float renders a tilde.
  readonly estimated: boolean;
  readonly firstRun: boolean;
}

// Roughly four characters to a token across English prose and JSON alike.
// Deliberately crude: this branch only runs when nothing measured is
// available, and a more elaborate guess would still be a guess while looking
// like arithmetic.
const CHARS_PER_TOKEN = 4;

export function floatFor(scopeId: string, requestChars: number): TokenFloat {
  const history = readAverage(scopeId);
  if (history !== null) {
    return { tokens: Math.round(history.avg), estimated: false, firstRun: false };
  }
  const guess = Math.max(1, Math.round(requestChars / CHARS_PER_TOKEN));
  return { tokens: guess, estimated: true, firstRun: true };
}

// The words the float and the popover's status line both use, so the visible
// number and what a screen reader hears are the same sentence.
export function describeFloat(float: TokenFloat): string {
  if (float.firstRun) return `First run for this section -- about ${float.tokens} tokens`;
  return `${float.tokens} tokens`;
}
