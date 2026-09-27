import { describe, expect, it } from "vitest";

import {
  disagrees,
  feedbackFromObservation,
  newestVerdict,
  targetKey,
  validatorFor,
  validatorFromObservation,
  validatorSentence,
} from "../../src/apps/nexus/feedback";
import { anyLabelled, effectiveReuse, reuseTally } from "../../src/apps/nexus/reuse";
import {
  observationFromRow,
  overrideFrom,
  parseHead,
  rerunFrom,
  runFromRow,
  stepFromRow,
  validationFrom,
} from "../../src/apps/nexus/rows";
import {
  BRANCH_CONSEQUENCE,
  answeredBy,
  askedFor,
  composerArgs,
  currentVersionOf,
  freshDraft,
  nextVersion,
  overrideFacts,
  parseInputValue,
  rerunConsequence,
  stepVersionFromRow,
  stepsAfter,
  versionCount,
  versionsByKey,
  versionsOf,
  versionsSignature,
  type ComposerDraft,
} from "../../src/apps/nexus/versions";
import {
  constructRow,
  feedbackObservation,
  runRow,
  stepRow,
  stepVersionRow,
  validatorObservation,
} from "./harness";

// The pure decisions behind stepping into a run (epic memql#5414). Each is a
// function of rows, asserted here with no browser, no cluster and no React.

describe("the run's head, re-run and validation", () => {
  it("reads the head tolerantly, and never invents a version 0", () => {
    expect(
      parseHead({
        draft: { version: 3 },
        fetch: { version: 1, runId: "run-src" },
        bad: { version: 0 },
        worse: { version: "2" },
        "": { version: 1 },
      }),
    ).toEqual({ draft: { version: 3, runId: "" }, fetch: { version: 1, runId: "run-src" } });
    expect(parseHead(undefined)).toEqual({});
  });

  it("reads `{}` and an absent re-run as NONE, because a truthy empty object would hide every act", () => {
    expect(rerunFrom({})).toBeNull();
    expect(rerunFrom(undefined)).toBeNull();
    expect(rerunFrom({ requestId: "r1", reason: "rerun", stepKey: "draft", requestedAt: "t" })).toEqual({
      requestId: "r1",
      reason: "rerun",
      stepKey: "draft",
      requestedAt: "t",
    });
  });

  it("reads the validator's summary, and nothing without a verdict", () => {
    expect(validationFrom({ verdict: "flag", stepKey: "draft", version: 3, observationId: "o1" })).toMatchObject({
      verdict: "flag",
      stepKey: "draft",
      version: 3,
      observationId: "o1",
    });
    expect(validationFrom({})).toBeNull();
    expect(runFromRow(runRow({ id: "r1" })).validation).toBeNull();
  });
});

describe("a step's versions", () => {
  it("strips the reply's `@v<version>` suffix so a version names its step row", () => {
    const version = stepVersionFromRow({ ...stepVersionRow({ id: "x", key: "draft", seq: 2, version: 2 }), id: "run-1-draft@v2" });
    expect(version.id).toBe("run-1-draft");
    expect(version.version).toBe(2);
    expect(version.current).toBe(true);
    // A payload that did not repeat the version is read off the suffix.
    const bare = stepVersionFromRow({ id: "run-1-draft@v4", key: "draft", seq: 2, status: "done" });
    expect(bare.version).toBe(4);
  });

  it("falls back to the attempt for a row written before `version` existed", () => {
    expect(stepFromRow(stepRow({ id: "s", key: "a", seq: 0, attempt: 3 })).version).toBe(3);
    expect(stepFromRow(stepRow({ id: "s", key: "a", seq: 0, attempt: 3, version: 2 })).version).toBe(2);
  });

  it("groups by key, oldest first, one entry per version", () => {
    const versions = [3, 1, 2].map((v) =>
      stepVersionFromRow(stepVersionRow({ id: "run-1-draft", key: "draft", seq: 2, version: v })),
    );
    const byKey = versionsByKey(versions);
    expect(byKey.get("draft")?.map((v) => v.version)).toEqual([1, 2, 3]);
  });

  it("lays the live row over the version it carries, and keeps the read's others", () => {
    const read = versionsByKey(
      [1, 2].map((v) => stepVersionFromRow(stepVersionRow({ id: "run-1-draft", key: "draft", seq: 2, version: v, status: "done" }))),
    );
    const live = stepFromRow(stepRow({ id: "run-1-draft", key: "draft", seq: 2, version: 3, attempt: 3, status: "running" }));
    expect(versionsOf(read, live, "draft").map((v) => `${v.version}:${v.status}`)).toEqual(["1:done", "2:done", "3:running"]);
  });

  it("takes the live head first, skips a branch's borrowed entry, then the read's mark, then the newest", () => {
    const versions = [1, 2, 3].map((v) =>
      stepVersionFromRow(stepVersionRow({ id: "run-1-draft", key: "draft", seq: 2, version: v, current: v === 2 })),
    );
    const run = (head: Record<string, unknown>) => runFromRow(runRow({ id: "run-1", head }));
    expect(currentVersionOf(run({ draft: { version: 1 } }), versions, "draft")).toBe(1);
    expect(currentVersionOf(run({ draft: { version: 1, runId: "run-src" } }), versions, "draft")).toBe(2);
    expect(currentVersionOf(run({}), versions, "draft")).toBe(2);
    const unmarked = versions.map((v) => ({ ...v, current: null }));
    expect(currentVersionOf(run({}), unmarked, "draft")).toBe(3);
  });

  it("counts the versions anything on the page knows of, and runs as one past them", () => {
    const head = parseHead({ draft: { version: 2 } });
    const live = stepFromRow(stepRow({ id: "s", key: "draft", seq: 2, version: 2, attempt: 2 }));
    expect(versionCount([], head, live, "draft")).toBe(2);
    expect(nextVersion([], head, live, "draft")).toBe(3);
    const read = [1, 2, 3, 4].map((v) => stepVersionFromRow(stepVersionRow({ id: "s", key: "draft", seq: 2, version: v })));
    expect(nextVersion(read, head, live, "draft")).toBe(5);
  });

  it("re-reads when a version appears or the head moves, and NOT on a status flip", () => {
    const run = runFromRow(runRow({ id: "r", head: { draft: { version: 2 } } }));
    const done = [stepFromRow(stepRow({ id: "s", key: "draft", seq: 0, version: 2, status: "done" }))];
    const running = [stepFromRow(stepRow({ id: "s", key: "draft", seq: 0, version: 2, status: "running" }))];
    const newer = [stepFromRow(stepRow({ id: "s", key: "draft", seq: 0, version: 3, status: "running" }))];
    expect(versionsSignature(run, done)).toBe(versionsSignature(run, running));
    expect(versionsSignature(run, newer)).not.toBe(versionsSignature(run, done));
    const moved = runFromRow(runRow({ id: "r", head: { draft: { version: 1 } } }));
    expect(versionsSignature(moved, done)).not.toBe(versionsSignature(run, done));
  });
});

describe("what a version was asked for and what answered it", () => {
  it("names the override's level, model and effort, and nothing when there is none", () => {
    expect(askedFor(overrideFrom({ level: "reasoning", model: "app:claude-code:opus", effort: "xhigh" }))).toBe(
      "Reasoning, app:claude-code:opus, extra high effort",
    );
    expect(askedFor(overrideFrom({}))).toBe("");
  });

  it("names only what the binding recorded", () => {
    expect(answeredBy({ provider: "anthropic", model: "claude-sonnet-4-5" })).toBe("anthropic, claude-sonnet-4-5");
    expect(answeredBy({ skillIds: [] })).toBe("");
    expect(answeredBy(null)).toBe("");
  });

  it("says what a person wrote in their words, and what rode along", () => {
    const facts = overrideFacts(
      overrideFrom({
        prompt: "Lead with the regional totals.",
        inputs: { month: "2026-08" },
        guidance: { axes: { process: true }, reason: "It skipped the returns ledger." },
      }),
      false,
    );
    expect(facts.map((f) => [f.label, f.value])).toEqual([
      ["Instructions", "Lead with the regional totals."],
      ["Input", "month = 2026-08"],
      ["Passed on", "What was wrong with the approach -- It skipped the returns ledger."],
    ]);
    expect(overrideFacts(overrideFrom({ prompt: "Do it all." }), true)[0]?.label).toBe("Prompt");
    // `requestedBy` alone changes nothing, so it is not a fact.
    expect(overrideFacts(overrideFrom({ requestedBy: "me" }), false)).toEqual([]);
  });
});

describe("the consequence, said before the act", () => {
  it("counts the steps after the one re-run, in the run's own order", () => {
    expect(stepsAfter(["fetch", "draft", "publish"], [], "draft")).toBe(1);
    expect(stepsAfter([], ["fetch", "draft", "publish", "notify"], "draft")).toBe(2);
    expect(stepsAfter(["a"], ["a"], "a")).toBe(0);
  });

  it("says the version it runs as and what follows it", () => {
    expect(rerunConsequence(4, 2)).toBe("Runs as version 4. The 2 steps after it run again.");
    expect(rerunConsequence(2, 1)).toBe("Runs as version 2. The step after it runs again.");
    expect(rerunConsequence(2, 0)).toBe("Runs as version 2. Nothing after it runs again.");
    expect(BRANCH_CONSEQUENCE).toBe(
      "Opens a new run from this step. The steps before it are reused, not run again.",
    );
  });
});

describe("the composer's payload: only what was changed", () => {
  const plain = { session: false, prompt: "" };

  it("sends nothing for a draft nobody touched", () => {
    expect(composerArgs(freshDraft(null, false), plain)).toEqual({});
  });

  it("sends each field only when it moved", () => {
    const draft: ComposerDraft = { ...freshDraft(null, false), level: "reasoning", model: "  fleet:qwen3:8b ", text: "  Be brief. " };
    expect(composerArgs(draft, plain)).toEqual({ level: "reasoning", model: "fleet:qwen3:8b", prompt: "Be brief." });
    expect(composerArgs({ ...freshDraft(null, false), effort: "high" }, plain)).toEqual({ effort: "high" });
  });

  it("sends a session prompt only when it differs from the recorded one", () => {
    const session = { session: true, prompt: "Open the workbook." };
    expect(composerArgs({ ...freshDraft(null, true), text: "Open the workbook." }, session)).toEqual({});
    expect(composerArgs({ ...freshDraft(null, true), text: "Open the workbook and the ledger." }, session)).toEqual({
      prompt: "Open the workbook and the ledger.",
    });
    // A prompt not yet read is never sent as if it had been edited.
    expect(composerArgs(freshDraft(null, true), session)).toEqual({});
  });

  it("sends an input only when its value moved or the person added it", () => {
    const draft = freshDraft({ month: "2026-08", region: "eu" }, false);
    expect(composerArgs(draft, plain)).toEqual({});
    const edited: ComposerDraft = {
      ...draft,
      inputs: [
        ...draft.inputs.map((row) => (row.key === "month" ? { ...row, value: "2026-09" } : row)),
        { id: "added", key: "limit", value: "20", original: null },
        { id: "blank", key: "  ", value: "x", original: null },
      ],
    };
    expect(composerArgs(edited, plain)).toEqual({ inputs: { month: "2026-09", limit: 20 } });
  });

  it("reads a value as JSON only when it plainly is", () => {
    expect(parseInputValue("42")).toBe(42);
    expect(parseInputValue("true")).toBe(true);
    expect(parseInputValue('["a", "b"]')).toEqual(["a", "b"]);
    expect(parseInputValue("2026-08")).toBe("2026-08");
    expect(parseInputValue("hello")).toBe("hello");
    expect(parseInputValue("null")).toBe("null");
    expect(parseInputValue("{not json")).toBe("{not json");
  });
});

describe("verdicts and the validator", () => {
  it("reads a person's verdict in D's shape, and a run-level one with no step", () => {
    const step = feedbackFromObservation(
      observationFromRow(
        feedbackObservation({ id: "f1", verdict: "dislike", stepKey: "draft", version: 2, axes: { process: true }, reason: "No ledger." }),
      ),
    );
    expect(step).toMatchObject({
      verdict: "dislike",
      axes: { product: false, process: true, performance: false },
      reason: "No ledger.",
      target: { stepKey: "draft", version: 2 },
    });
    const run = feedbackFromObservation(observationFromRow(feedbackObservation({ id: "f2", verdict: "like" })));
    expect(run?.target).toEqual({ stepKey: "", version: null });
    expect(targetKey(run!.target)).toBe("run");
    expect(targetKey(step!.target)).toBe("draft@v2");
  });

  it("reads only a decision that carries a validator block as the validator", () => {
    const flagged = validatorFromObservation(
      observationFromRow(validatorObservation({ id: "v1", verdict: "flag", stepKey: "draft", version: 3, axes: { product: true }, reason: "Wrong total." })),
    );
    expect(flagged).toMatchObject({ verdict: "flag", target: { stepKey: "draft", version: 3 }, reason: "Wrong total." });
    const otherDecision = validatorFromObservation(
      observationFromRow({ id: "d1", kind: "decision", data: { branch: "left" }, createdAt: "t" }),
    );
    expect(otherDecision).toBeNull();
  });

  it("takes the NEWEST verdict per target, and a later write wins a tie", () => {
    const entries = [
      feedbackFromObservation(observationFromRow(feedbackObservation({ id: "a", verdict: "like", stepKey: "draft", version: 2, createdAt: "2026-09-01T10:00:00Z" })))!,
      feedbackFromObservation(observationFromRow(feedbackObservation({ id: "b", verdict: "dislike", stepKey: "draft", version: 2, axes: { product: true }, createdAt: "2026-09-01T11:00:00Z" })))!,
      feedbackFromObservation(observationFromRow(feedbackObservation({ id: "c", verdict: "neutral", stepKey: "draft", version: 3, createdAt: "2026-09-01T12:00:00Z" })))!,
    ];
    expect(newestVerdict(entries, { stepKey: "draft", version: 2 })?.id).toBe("b");
    expect(newestVerdict(entries, { stepKey: "draft", version: 3 })?.id).toBe("c");
    expect(newestVerdict(entries, { stepKey: "", version: null })).toBeNull();
    const tie = [...entries, { ...entries[1]!, id: "d", verdict: "like" as const }];
    expect(newestVerdict(tie, { stepKey: "draft", version: 2 })?.id).toBe("d");
  });

  it("follows the run's own pointer to the validator's decision", () => {
    const one = validatorFromObservation(observationFromRow(validatorObservation({ id: "v1", verdict: "pass", stepKey: "a", version: 1, createdAt: "2026-09-01T10:00:00Z" })))!;
    const two = validatorFromObservation(observationFromRow(validatorObservation({ id: "v2", verdict: "flag", stepKey: "a", version: 1, createdAt: "2026-09-01T09:00:00Z" })))!;
    expect(validatorFor([one, two], validationFrom({ verdict: "flag", observationId: "v2" }))?.id).toBe("v2");
    expect(validatorFor([one, two], null)?.id).toBe("v1");
  });

  it("disagrees on a like of a flagged answer or a dislike of a passed one -- never on neutral", () => {
    expect(disagrees("like", "flag")).toBe(true);
    expect(disagrees("dislike", "pass")).toBe(true);
    expect(disagrees("like", "pass")).toBe(false);
    expect(disagrees("dislike", "flag")).toBe(false);
    expect(disagrees("neutral", "flag")).toBe(false);
    expect(disagrees("neutral", "pass")).toBe(false);
    expect(disagrees(null, "flag")).toBe(false);
  });

  it("says what the validator found in words, and a flag it has not read as a problem", () => {
    const flagged = validatorFromObservation(
      observationFromRow(validatorObservation({ id: "v", verdict: "flag", stepKey: "draft", version: 3, axes: { process: true }, reason: "It never opened the ledger." })),
    );
    expect(validatorSentence("flag", flagged)).toBe(
      "Checked before you saw it: flagged the approach -- It never opened the ledger.",
    );
    expect(validatorSentence("pass", null)).toBe("Checked before you saw it: no problems found.");
    expect(validatorSentence("flag", null, "draft")).toBe("Checked before you saw it: flagged a problem in draft.");
  });
});

describe("reuse labels", () => {
  it("lets a person's label win, and an empty one hand the label back to the evidence", () => {
    expect(effectiveReuse(constructRow({ id: "c", name: "a", reuse: "goalSpecific", reuseOverride: { label: "reusable" } }))).toBe("reusable");
    expect(effectiveReuse(constructRow({ id: "c", name: "a", reuse: "goalSpecific", reuseOverride: { label: "" } }))).toBe("goalSpecific");
    expect(effectiveReuse(constructRow({ id: "c", name: "a" }))).toBe("");
  });

  it("counts automations once each, leaves retired ones out, and keeps unlabelled apart", () => {
    const tally = reuseTally([
      constructRow({ id: "c1", name: "a", reuse: "reusable" }),
      constructRow({ id: "v1:authoring:construct:c1", name: "a", reuse: "reusable" }),
      constructRow({ id: "c2", name: "b", reuse: "goalSpecific" }),
      constructRow({ id: "c3", name: "c", reuse: "accountSpecific" }),
      constructRow({ id: "c4", name: "d" }),
      constructRow({ id: "c5", name: "e", reuse: "reusable", status: "retired" }),
      constructRow({ id: "c6", name: "f", kind: "query", reuse: "reusable" }),
    ]);
    expect(tally).toEqual({ total: 4, reusable: 1, goalSpecific: 1, accountSpecific: 1, unlabelled: 1 });
    expect(anyLabelled(tally)).toBe(true);
    expect(anyLabelled(reuseTally([constructRow({ id: "c", name: "x" })]))).toBe(false);
  });
});
