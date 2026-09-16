import { describe, expect, it } from "vitest";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  FLOOR_RULE_SENTENCE,
  LOCKED_RULE_SENTENCE,
  RULE_WHEN_KEYS,
  isFloorRule,
  precedenceTaken,
  ruleFromRow,
  ruleSentence,
  rulesInOrder,
  simulationSentence,
  whenObjectFrom,
  type RuleRow,
  type RuleWhenKey,
} from "../../src/apps/settings/rulesFacts";

// A rule's conditions, as arithmetic (epic memql#5153, D1).
//
// ===========================================================================
// THE ONE ASSERTION THIS FILE EXISTS FOR
// ===========================================================================
// The engine keeps three states apart, per condition:
//
//   key ABSENT        -> no condition. The rule does not look at this at all.
//   key present, ""   -> a condition matching ONLY an empty value.
//   key present, "x"  -> a condition matching "x".
//
// A form has one obvious implementation -- seven string fields, send all
// seven -- and it collapses the first state into the second. Every field the
// person left alone then arrives as a condition almost nothing satisfies, so
// the rule SILENTLY NEVER FIRES: nothing errors, nothing logs, and the rule
// sits in the list reading exactly as it was meant to.
//
// The difference is observable from this side in one place only -- the KEY
// SET of the object `whenObjectFrom` builds -- so that is what is asserted,
// with `Object.keys` and `in`. `expect(out.prompt).toBeUndefined()` would
// pass for a key that is PRESENT and holds undefined, which is the exact
// shape being refused, so it is not used here and should not be added.
//
// ===========================================================================
// AND THE HALF THAT MAKES A RULE FALSIFIABLE
// ===========================================================================
// `simulationSentence` over ZERO decisions must not read as "changed
// nothing". "Nothing was compared" and "nothing would change" lead to
// opposite confidence, and only one of them is a reason to activate.

function rule(over: Partial<RuleRow> = {}): RuleRow {
  return {
    name: "aRule",
    when: {},
    level: "",
    policy: "localFirst",
    precedence: 10,
    onUnavailable: "",
    excludes: [],
    locked: false,
    described: "",
    ...over,
  };
}

/** Every condition key except the ones named -- the "untouched" set. */
function othersThan(...set: RuleWhenKey[]): RuleWhenKey[] {
  return RULE_WHEN_KEYS.filter((k) => !set.includes(k));
}

describe("building the `when` object a form sends", () => {
  it("emits NO KEY AT ALL for a condition nobody ticked", () => {
    const out = whenObjectFrom(
      // A draft can hold a value for a field the person later UNTICKED. It is
      // the tick that decides, never the leftover text.
      { level: "reasoning", prompt: "agentReply", role: "operator" },
      new Set<RuleWhenKey>(["level"]),
    );
    expect(Object.keys(out)).toEqual(["level"]);
    expect(out).toEqual({ level: "reasoning" });
    for (const key of othersThan("level")) {
      // `in`, not `=== undefined`: a present key holding undefined is exactly
      // the defect, and the loose check passes for it.
      expect(key in out).toBe(false);
    }
    expect("prompt" in out).toBe(false);
  });

  it("emits the key WITH an empty value for a condition ticked and left blank", () => {
    // A person who ticks a condition and types nothing has said "match an
    // empty value", which is a real and different rule. Dropping the key
    // would silently widen their rule to every call.
    const out = whenObjectFrom({}, new Set<RuleWhenKey>(["role"]));
    expect(Object.keys(out)).toEqual(["role"]);
    expect("role" in out).toBe(true);
    expect(out.role).toBe("");
  });

  it("trims a value, and reads whitespace as the blank it looks like", () => {
    expect(whenObjectFrom({ tag: "  nightly  " }, new Set<RuleWhenKey>(["tag"]))).toEqual({
      tag: "nightly",
    });
    const blank = whenObjectFrom({ tag: "   " }, new Set<RuleWhenKey>(["tag"]));
    // Still a CONDITION -- the tick is what makes it one -- just an empty one.
    expect("tag" in blank).toBe(true);
    expect(blank.tag).toBe("");
  });

  it("emits nothing at all when nothing is ticked", () => {
    const out = whenObjectFrom({ level: "fast", prompt: "agentReply" }, new Set<RuleWhenKey>());
    expect(Object.keys(out)).toEqual([]);
    expect(out).toEqual({});
  });

  it("keeps `role` and `actorRole` as two conditions, never one", () => {
    // An operator WATCHING a non-operator agent work is not an operator turn.
    // Collapsing the pair routes on who is looking rather than on what is
    // acting, and the two answers differ on exactly the calls that matter.
    const out = whenObjectFrom(
      { role: "operator", actorRole: "developer" },
      new Set<RuleWhenKey>(["role", "actorRole"]),
    );
    expect(out).toEqual({ role: "operator", actorRole: "developer" });
    expect(Object.keys(out).sort()).toEqual(["actorRole", "role"]);
  });
});

describe("reading a rule back off the wire", () => {
  it("reads a condition that is present AND EMPTY as present", () => {
    // The round trip is the whole point: a rule must display as the rule that
    // is actually running. A truthiness check here would show the person a
    // DIFFERENT rule from the one the engine is evaluating.
    const read = ruleFromRow({ name: "r", when: { role: "" }, policy: "localFirst" } as Row);
    expect("role" in read.when).toBe(true);
    expect(read.when.role).toBe("");
    expect(ruleSentence(read)).toMatch(/the role acting is empty/);
  });

  it("reads a key the engine never wrote as absent", () => {
    const read = ruleFromRow({ name: "r", when: { level: "fast" }, policy: "localOnly" } as Row);
    expect(Object.keys(read.when)).toEqual(["level"]);
    expect("prompt" in read.when).toBe(false);
  });

  it("survives the whole round trip -- form to wire to list", () => {
    const sent = whenObjectFrom(
      { level: "reasoning", role: "" },
      new Set<RuleWhenKey>(["level", "role"]),
    );
    const back = ruleFromRow({ name: "r", when: sent, policy: "localOnly" } as Row);
    expect(Object.keys(back.when).sort()).toEqual(["level", "role"]);
    expect(back.when).toEqual({ level: "reasoning", role: "" });
  });

  it("reads a precedence the wire sent as a string", () => {
    expect(ruleFromRow({ name: "r", precedence: "60" } as Row).precedence).toBe(60);
    expect(ruleFromRow({ name: "r" } as Row).precedence).toBe(0);
  });
});

describe("the order the engine tries them in", () => {
  it("puts every shipped rule first, then sorts by precedence highest-first", () => {
    const rules = [
      rule({ name: "mine10", precedence: 10 }),
      rule({ name: "shippedLow", precedence: 1, locked: true, when: { level: "fast" } }),
      rule({ name: "mine90", precedence: 90 }),
      rule({ name: "shippedHigh", precedence: 60, locked: true, when: { role: "operator" } }),
    ];
    // shippedLow sits at precedence 1 and still runs before mine90 at 90 --
    // which is what makes this discriminating. A plain precedence sort gives
    // the opposite answer.
    expect(rulesInOrder(rules).map((r) => r.name)).toEqual([
      "shippedHigh",
      "shippedLow",
      "mine90",
      "mine10",
    ]);
  });

  it("sends the FLOOR last, shipped though it is", () => {
    // A locked rule that states no conditions matches every call. Drawn in
    // the locked partition it would sit above every authored rule -- a
    // picture of the bug memql#5127 fixed, read as authoritative: somebody
    // would conclude their own rules never run.
    const floor = rule({ name: "defaultRule", precedence: 0, locked: true, when: {} });
    const rules = [floor, rule({ name: "mine", precedence: 10 })];
    expect(isFloorRule(floor)).toBe(true);
    expect(rulesInOrder(rules).map((r) => r.name)).toEqual(["mine", "defaultRule"]);
    // And it is recognised by IDENTITY, not by name: a shipped rule WITH
    // conditions is an ordinary locked rule however it is spelled.
    expect(isFloorRule(rule({ name: "defaultRule", locked: true, when: { level: "fast" } }))).toBe(
      false,
    );
    // An unlocked rule with no conditions is not the floor either -- it is
    // somebody's catch-all, and it obeys precedence like everything they wrote.
    expect(isFloorRule(rule({ name: "mineCatchAll", locked: false, when: {} }))).toBe(false);
  });

  it("breaks a precedence tie by name, so two replicas draw the same list", () => {
    const rules = [rule({ name: "bravo", precedence: 5 }), rule({ name: "alpha", precedence: 5 })];
    expect(rulesInOrder(rules).map((r) => r.name)).toEqual(["alpha", "bravo"]);
  });

  it("does not reorder the array it was handed", () => {
    const rules = [rule({ name: "mine", precedence: 1 }), rule({ name: "shipped", precedence: 1, locked: true, when: { level: "fast" } })];
    rulesInOrder(rules);
    expect(rules.map((r) => r.name)).toEqual(["mine", "shipped"]);
  });
});

describe("a rule as one line of English", () => {
  it("reads a rule with no conditions as every call", () => {
    expect(ruleSentence(rule({ policy: "localFirst" }))).toBe("Every call, try localFirst.");
  });

  it("reads one condition", () => {
    expect(ruleSentence(rule({ when: { level: "reasoning" }, policy: "localOnly" }))).toBe(
      "When the level asked for is reasoning, try localOnly.",
    );
  });

  it("reads several, joined the way a person would say them", () => {
    const said = ruleSentence(
      rule({
        when: { level: "reasoning", prompt: "agentReply", role: "operator" },
        policy: "localFirst",
      }),
    );
    expect(said).toBe(
      "When the level asked for is reasoning, the prompt is agentReply and " +
        "the role acting is operator, try localFirst.",
    );
    // Two conditions take "and" with no comma before it.
    expect(ruleSentence(rule({ when: { level: "fast", tag: "nightly" }, policy: "localOnly" }))).toBe(
      "When the level asked for is fast and the call's tag is nightly, try localOnly.",
    );
  });

  it("says what the rule DOES, not only what it looks at", () => {
    expect(
      ruleSentence(rule({ level: "reasoning", policy: "localFirst", onUnavailable: "degrade" })),
    ).toBe("Every call, ask for reasoning and try localFirst. If nothing there is available, step down a level.");
    expect(ruleSentence(rule({ policy: "localOnly", onUnavailable: "park" }))).toMatch(
      /park and wait for a person/,
    );
  });

  it("says a rule with no policy has no policy, rather than trailing off", () => {
    expect(ruleSentence(rule({ policy: "" }))).toBe("Every call, try no policy.");
  });
});

describe("a precedence somebody else already holds", () => {
  it("names the rule that holds it", () => {
    const rules = [rule({ name: "planningStaysLocal", precedence: 30 })];
    expect(precedenceTaken(rules, 30, "newRule")?.name).toBe("planningStaysLocal");
    expect(precedenceTaken(rules, 31, "newRule")).toBeNull();
  });

  it("ignores shipped rules, whose numbers a custom rule may reuse", () => {
    // Shipped rules do not compete with authored ones for a slot -- they run
    // in their own partition -- so reporting a collision would refuse a
    // number that is genuinely free.
    const rules = [rule({ name: "operatorReasoning", precedence: 60, locked: true, when: { role: "operator" } })];
    expect(precedenceTaken(rules, 60, "mine")).toBeNull();
  });

  it("ignores the rule's own name, so editing one does not collide with itself", () => {
    const rules = [rule({ name: "planningStaysLocal", precedence: 30 })];
    expect(precedenceTaken(rules, 30, "planningStaysLocal")).toBeNull();
  });
});

describe("what a simulation is allowed to claim", () => {
  it("refuses to read ZERO decisions as 'this would change nothing'", () => {
    // "0 of 0" and "0 of 400" are opposite confidence signals, and a person
    // who reads the first as a safety result has been told the reverse of the
    // truth. The sentence has to say there was nothing to compare against.
    const said = simulationSentence({ considered: 0, changed: 0, refusal: "" });
    expect(said).toMatch(/nothing to compare/);
    expect(said).toMatch(/not the same as it changing nothing/);
    expect(said).not.toMatch(/changed none of them/);
  });

  it("says so plainly when there WAS something to compare against", () => {
    // The control for the line above: without it, "does not say 'changed
    // none'" would pass on a function that can never say it.
    expect(simulationSentence({ considered: 400, changed: 0, refusal: "" })).toBe(
      "Over the last 400 decisions this would have changed none of them.",
    );
    expect(simulationSentence({ considered: 400, changed: 12, refusal: "" })).toBe(
      "Over the last 400 decisions this would have changed 12.",
    );
  });

  it("hands back the cluster's own refusal instead of a count", () => {
    // A refusal means nothing was compared. Rendering "0 of 0" over it would
    // turn a failure into a reassuring number.
    expect(
      simulationSentence({ considered: 0, changed: 0, refusal: "routingRuleSimulate is owner-only" }),
    ).toBe("routingRuleSimulate is owner-only");
  });
});

describe("what the screen says about a rule it cannot edit", () => {
  it("explains that a matching shipped rule takes priority", () => {
    expect(LOCKED_RULE_SENTENCE).toMatch(/cannot be edited or removed/);
    expect(LOCKED_RULE_SENTENCE).toMatch(/matching shipped rule takes priority/);
  });

  it("says what the floor is FOR, so its place at the bottom does not read as a sorting bug", () => {
    expect(FLOOR_RULE_SENTENCE).toMatch(/runs last/);
    expect(FLOOR_RULE_SENTENCE).toMatch(/never fall through/);
  });
});
