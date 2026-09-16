import { useCallback, useEffect, useState } from "react";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import { LEVELS, type LevelId } from "./routingFacts";

// Rules, as facts (epic memql#5153, D1) -- and the arithmetic that keeps a UI
// from destroying a distinction the engine preserves all the way down.
//
// ===========================================================================
// THE ONE THING IN THIS FILE THAT MUST NOT BE GOT WRONG
// ===========================================================================
// A rule's `@when` is an OBJECT of condition keys, and three states have to
// survive the trip from a form to the wire:
//
//   key absent          -> no condition. The rule does not look at this at all.
//   key present, ""     -> a condition matching ONLY an empty value.
//   key present, "x"    -> a condition matching "x".
//
// A form has one obvious implementation -- six string fields, send all six --
// and it collapses the first two into the second. Every field the person left
// alone arrives as a condition nothing satisfies, so the rule silently never
// fires. Nothing errors, nothing logs, and the rule sits in the list looking
// correct.
//
// `whenObjectFrom` is the only way this app builds that object, and it emits a
// key only where the person actually set one. `whenObjectFrom` has a test
// asserting an untouched field produces NO KEY, because that assertion is the
// only place the difference is observable from this side.

/** The closed set of things a rule may look at. Engine-aligned. */
export const RULE_WHEN_KEYS = [
  "level",
  "modality",
  "prompt",
  "role",
  "actorRole",
  "tag",
  "touches",
] as const;

export type RuleWhenKey = (typeof RULE_WHEN_KEYS)[number];

/**
 * What each condition asks, in the reader's terms.
 *
 * `role` and `actorRole` are the pair worth spelling out. They are different
 * questions -- an operator watching a non-operator agent work is not an
 * operator turn -- and collapsing them routes on who is WATCHING rather than
 * on what is ACTING.
 */
/**
 * ONE STRING PER CONDITION, and it has to survive two jobs: standing alone
 * beside a checkbox in the fields panel, and being dropped into
 * "When <it> is <value>" in the rule's sentence.
 *
 * A second, longer set was tried for the checkboxes and removed. It read
 * marginally better in isolation and cost a whole class of drift -- the two
 * immediately disagreed, and a test looking up a checkbox by the sentence's
 * wording could not find it. Every phrase here is therefore a NOUN PHRASE that
 * reads as English in the sentence: "which prompt is running" produced "When
 * which prompt is running is agentReply", which is not English and was on the
 * screen before the pixels were looked at.
 */
export const RULE_WHEN_MEANING: Record<RuleWhenKey, string> = {
  level: "the level asked for",
  modality: "the kind of call",
  prompt: "the prompt",
  role: "the role acting",
  actorRole: "the role watching",
  tag: "the call's tag",
  touches: "what the call touches",
};


export interface RuleRow {
  revision?: number;
  name: string;
  /** The conditions, exactly as stored. An absent key is no condition. */
  when: Partial<Record<RuleWhenKey, string>>;
  /** A level the rule raises the call to, or "" for "leave it alone". */
  level: string;
  policy: string;
  precedence: number;
  onUnavailable: string;
  excludes: string[];
  /** Shipped with the engine: evaluates first, cannot be edited or removed. */
  locked: boolean;
  /** Free text the person wrote, when the rule came from a sentence. */
  described: string;
}

function str(row: Record<string, unknown>, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function whenFrom(raw: unknown): Partial<Record<RuleWhenKey, string>> {
  const out: Partial<Record<RuleWhenKey, string>> = {};
  if (raw === null || typeof raw !== "object") return out;
  const obj = raw as Record<string, unknown>;
  for (const key of RULE_WHEN_KEYS) {
    // `in` rather than a truthiness check: a key present and EMPTY is a real
    // condition, and reading it back as absent would show the person a rule
    // different from the one that is running.
    if (key in obj && typeof obj[key] === "string") out[key] = obj[key] as string;
  }
  return out;
}

export function ruleFromRow(raw: Row): RuleRow {
  const r = flatten(raw as Record<string, unknown>);
  const precedence = r["precedence"];
  const excludes = r["excludes"];
  return {
    revision: typeof r["revision"] === "number" ? r["revision"] : undefined,
    name: str(r, "name"),
    when: whenFrom(r["when"]),
    level: str(r, "level"),
    policy: str(r, "policy"),
    precedence:
      typeof precedence === "number"
        ? precedence
        : typeof precedence === "string" && precedence.trim() !== ""
          ? Number(precedence)
          : 0,
    onUnavailable: str(r, "onUnavailable"),
    excludes: Array.isArray(excludes) ? excludes.filter((e): e is string => typeof e === "string") : [],
    locked: r["locked"] === true,
    described: str(r, "described"),
  };
}

/**
 * Build the `when` object from a form's draft.
 *
 * A key is emitted only where `set` says the person touched that field. This
 * is what preserves "no condition" against "matches only an empty value" --
 * see the header. A value is trimmed, but a field the person deliberately
 * emptied still emits its key with "", because that is a choice they made.
 */
export function whenObjectFrom(
  draft: Partial<Record<RuleWhenKey, string>>,
  set: ReadonlySet<RuleWhenKey>,
): Partial<Record<RuleWhenKey, string>> {
  const out: Partial<Record<RuleWhenKey, string>> = {};
  for (const key of RULE_WHEN_KEYS) {
    if (!set.has(key)) continue;
    out[key] = (draft[key] ?? "").trim();
  }
  return out;
}

/**
 * The FLOOR: a locked rule that states no conditions.
 *
 * It matches every call, and it is the one locked rule that does NOT evaluate
 * before the unlocked tier. That exemption is not a nicety -- without it the
 * floor is an absorbing state in the first partition and the entire authored
 * tier is dead, silently, with every rule still listed and nothing failing
 * (memql#5127). The engine exempts it by IDENTITY rather than by precedence,
 * because a low number is something an operator could promote.
 *
 * Recognised here the same way, by identity rather than by name, so a cluster
 * that renames its floor still gets a truthful list.
 */
export function isFloorRule(rule: RuleRow): boolean {
  return rule.locked && Object.keys(rule.when).length === 0;
}

/**
 * Rules in the order the engine evaluates them.
 *
 * Locked rules first, then precedence descending -- EXCEPT the floor, which
 * goes last. Sorting the floor by its locked-ness would draw it above every
 * custom rule, which is a picture of the bug epic memql#5127 fixed: a reader
 * would conclude their own rules never run, and they would be describing the
 * old behaviour rather than this one.
 */
export function rulesInOrder(rules: readonly RuleRow[]): RuleRow[] {
  return [...rules].sort((a, b) => {
    const aFloor = isFloorRule(a);
    const bFloor = isFloorRule(b);
    if (aFloor !== bFloor) return aFloor ? 1 : -1;
    if (a.locked !== b.locked) return a.locked ? -1 : 1;
    if (a.precedence !== b.precedence) return b.precedence - a.precedence;
    return a.name.localeCompare(b.name);
  });
}

/**
 * A rule as one line of English.
 *
 * The list is read top to bottom by somebody deciding whether the set does
 * what they meant, so each row has to be a claim they can agree or disagree
 * with -- not a row of field values they have to assemble into one.
 */
export function ruleSentence(rule: RuleRow): string {
  const conditions = RULE_WHEN_KEYS.filter((k) => k in rule.when).map((k) => {
    const value = rule.when[k] ?? "";
    if (value === "") return `${RULE_WHEN_MEANING[k]} is empty`;
    return `${RULE_WHEN_MEANING[k]} is ${value}`;
  });

  const head =
    conditions.length === 0
      ? "Every call"
      : conditions.length === 1
        ? `When ${conditions[0]}`
        : `When ${conditions.slice(0, -1).join(", ")} and ${conditions[conditions.length - 1]}`;

  const raise = rule.level === "" ? "" : ` ask for ${rule.level} and`;
  const policy = rule.policy === "" ? "no policy" : rule.policy;
  const tail =
    rule.onUnavailable === "park"
      ? " If nothing there is available, park and wait for a person."
      : rule.onUnavailable === "degrade"
        ? " If nothing there is available, step down a level."
        : "";

  return `${head},${raise} try ${policy}.${tail}`;
}

/**
 * Why a locked rule cannot be edited, and what to do instead.
 *
 * Said on the screen rather than left for somebody to discover as a refusal.
 * The engine's answer to "get this rule out of the way" is a custom rule above
 * it, and a person who does not know that reads an un-editable row as a bug.
 */
export const LOCKED_RULE_SENTENCE =
  "Shipped rules come back on every restart and cannot be edited or removed. " +
  "A matching shipped rule takes priority over custom rules. The shipped catch-all runs last.";

/**
 * What the floor is, said on its own row.
 *
 * It reads as the odd one out in the list -- locked, but at the bottom -- and
 * without a sentence that looks like a sorting mistake. It is the opposite: it
 * is what makes "a call that matches no rule" impossible by construction.
 */
export const FLOOR_RULE_SENTENCE =
  "The floor. It states no conditions, so it catches everything nothing above it " +
  "matched -- which is why it runs last, and why a call can never fall through.";

/** A draft precedence that would collide with an existing rule. */
export function precedenceTaken(rules: readonly RuleRow[], precedence: number, name: string): RuleRow | null {
  return rules.find((r) => r.precedence === precedence && r.name !== name && !r.locked) ?? null;
}

/** The levels a rule may raise a call to, plus "leave it alone". */
export const RULE_LEVEL_CHOICES: readonly string[] = ["", ...LEVELS];

export function isLevelId(value: string): value is LevelId {
  return (LEVELS as readonly string[]).includes(value);
}

// ===========================================================================
// SIMULATION
// ===========================================================================

export interface Simulation {
  validationOnly?: boolean;
  revision?: number;
  /** Decisions the simulation looked at. */
  considered: number;
  /** Of those, how many this rule would have changed. */
  changed: number;
  /** The engine's sentence when it could not simulate. Empty on success. */
  refusal: string;
}

/**
 * How the simulation reads.
 *
 * A simulation over ZERO decisions is not "would have changed nothing" -- it
 * is "there is nothing to check this against", and the two lead to opposite
 * confidence. A person who reads "would have changed 0 of 0" as a safety
 * signal has been told the opposite of the truth.
 */
export function simulationSentence(sim: Simulation): string {
  if (sim.refusal !== "") return sim.refusal;
  if (sim.validationOnly) return "The definition is valid against the current configuration. Historical decisions were not replayed.";
  if (sim.considered === 0) {
    return "There are no recorded decisions to check this against yet, so there is nothing to compare. That is not the same as it changing nothing.";
  }
  if (sim.changed === 0) {
    return `Over the last ${sim.considered} decisions this would have changed none of them.`;
  }
  return `Over the last ${sim.considered} decisions this would have changed ${sim.changed}.`;
}

// ===========================================================================
// THE READS
// ===========================================================================

/**
 * Probe the connection for a builtin.
 *
 * Every rules read and write lands with epic memql#5127. On a cluster without
 * it the generated client simply has no such method, and this answers null so
 * the surface can say "this cluster does not route by rules yet" -- which is
 * true, and is a different sentence from "you have no rules".
 */
function builtinOn(
  connection: ReturnType<typeof useOsConnection>,
  name: string,
):
  | ((args: Record<string, unknown>, opts?: unknown) => Promise<{ rows: () => Iterable<Row> }>)
  | null {
  if (connection === null) return null;
  const q = connection.query as unknown as Record<string, unknown>;
  const fn = q[name];
  return typeof fn === "function"
    ? (fn as (args: Record<string, unknown>, opts?: unknown) => Promise<{ rows: () => Iterable<Row> }>).bind(connection.query)
    : null;
}

export interface RulesState {
  rules: RuleRow[];
  loading: boolean;
  /** The cluster has the rules read at all. */
  supported: boolean;
  error: string;
  reload: () => void;
}

export function useRules(enabled: boolean): RulesState {
  const connection = useOsConnection();
  const [rules, setRules] = useState<RuleRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [supported, setSupported] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const read = builtinOn(connection, "routingRules");
    if (read === null) {
      setSupported(false);
      setRules([]);
      return;
    }
    setSupported(true);
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void read({}, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        setRules([...result.rows()].map(ruleFromRow));
      })
      .catch((err: unknown) => {
        if (stale) return;
        setRules([]);
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch]);

  return { rules, loading, supported, error, reload };
}

export interface RuleActionState {
  busy: boolean;
  failed: boolean;
  message: string;
}

export const IDLE_RULE_ACTION: RuleActionState = { busy: false, failed: false, message: "" };

export interface RuleActions {
  state: RuleActionState;
  /** Compile a sentence into a rule. Null when the cluster cannot compile. */
  describe: (sentence: string) => Promise<{ rule: RuleRow | null; problem: string }>;
  simulate: (rule: RuleRow) => Promise<Simulation>;
  activate: (rule: RuleRow) => Promise<boolean>;
  retire: (name: string, revision?: number) => Promise<boolean>;
  supported: boolean;
  clear: () => void;
}

export function useRuleActions(onChanged: () => void): RuleActions {
  const connection = useOsConnection();
  const [state, setState] = useState<RuleActionState>(IDLE_RULE_ACTION);
  const supported = builtinOn(connection, "routingRuleSave") !== null || builtinOn(connection, "routingRuleActivate") !== null;

  const clear = useCallback(() => setState(IDLE_RULE_ACTION), []);

  const describe = useCallback(
    async (sentence: string): Promise<{ rule: RuleRow | null; problem: string }> => {
      const compile = builtinOn(connection, "routingRuleDescribe");
      if (compile === null) {
        return { rule: null, problem: "This cluster cannot compile a rule from a sentence yet." };
      }
      try {
        const result = await compile({ sentence });
        const rows = [...result.rows()];
        if (rows.length === 0) return { rule: null, problem: "The compiler returned nothing." };
        const r = flatten(rows[0] as Record<string, unknown>);
        const problem = str(r, "problem");
        // THE COMPILER'S OWN SENTENCE IS THE ANSWER, not a rewrite of it. It
        // knows which clause it could not read; this file does not.
        if (problem !== "") return { rule: null, problem };
        return { rule: ruleFromRow(rows[0] as Row), problem: "" };
      } catch (err: unknown) {
        return { rule: null, problem: err instanceof Error ? err.message : String(err) };
      }
    },
    [connection],
  );

  const simulate = useCallback(
    async (rule: RuleRow): Promise<Simulation> => {
      const validate = builtinOn(connection, "routingRuleValidate");
      const run = validate ?? builtinOn(connection, "routingRuleSimulate");
      if (run === null) {
        return { considered: 0, changed: 0, refusal: "This cluster cannot simulate a rule yet." };
      }
      try {
        const result = await run({
          name: rule.name,
          ...(validate ? { conditions: rule.when, expectedRevision: rule.revision } : { when: rule.when }),
          excludes: rule.excludes,
          level: rule.level,
          policy: rule.policy,
          precedence: rule.precedence,
          onUnavailable: rule.onUnavailable,
        });
        const rows = [...result.rows()];
        if (rows.length === 0) return { considered: 0, changed: 0, refusal: "The cluster returned nothing." };
        const r = flatten(rows[0] as Record<string, unknown>);
        const considered = typeof r["considered"] === "number" ? (r["considered"] as number) : 0;
        const changed = typeof r["changed"] === "number" ? (r["changed"] as number) : 0;
        return { considered, changed, refusal: "", validationOnly: r["validationOnly"] === true, revision: typeof r["revision"] === "number" ? r["revision"] : undefined };
      } catch (err: unknown) {
        return {
          considered: 0,
          changed: 0,
          refusal: err instanceof Error ? err.message : String(err),
        };
      }
    },
    [connection],
  );

  const activate = useCallback(
    async (rule: RuleRow): Promise<boolean> => {
      const save = builtinOn(connection, "routingRuleSave");
      const write = save ?? builtinOn(connection, "routingRuleActivate");
      if (write === null) {
        setState({ busy: false, failed: true, message: "This cluster does not take custom rules yet." });
        return false;
      }
      setState({ busy: true, failed: false, message: "" });
      try {
        await write({
          name: rule.name,
          // The object, not six fields. See this file's header.
          ...(save ? { conditions: rule.when, expectedRevision: rule.revision } : { when: rule.when }),
          excludes: rule.excludes,
          level: rule.level,
          policy: rule.policy,
          precedence: rule.precedence,
          onUnavailable: rule.onUnavailable,
          description: rule.described,
        });
        setState({ busy: false, failed: false, message: `${rule.name} is active.` });
        onChanged();
        return true;
      } catch (err: unknown) {
        setState({
          busy: false,
          failed: true,
          message: err instanceof Error ? err.message : String(err),
        });
        return false;
      }
    },
    [connection, onChanged],
  );

  const retire = useCallback(
    async (name: string, revision?: number): Promise<boolean> => {
      const remove = builtinOn(connection, "routingRuleRemove");
      const write = remove ?? builtinOn(connection, "routingRuleRetire");
      if (write === null) {
        setState({ busy: false, failed: true, message: "This cluster does not take custom rules yet." });
        return false;
      }
      setState({ busy: true, failed: false, message: "" });
      try {
        if (remove && revision === undefined) throw new Error("Refresh the rules before removing one.");
        await write({ name, ...(remove ? { expectedRevision: revision } : {}) });
        setState({ busy: false, failed: false, message: `${name} is retired.` });
        onChanged();
        return true;
      } catch (err: unknown) {
        setState({
          busy: false,
          failed: true,
          message: err instanceof Error ? err.message : String(err),
        });
        return false;
      }
    },
    [connection, onChanged],
  );

  return { state, describe, simulate, activate, retire, supported, clear };
}
