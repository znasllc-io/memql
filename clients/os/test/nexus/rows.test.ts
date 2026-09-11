import { describe, expect, it } from "vitest";

import {
  approvalFromRow,
  approvalOptions,
  approvalSubjectLine,
  decisionLine,
  decisionsByStep,
  figure,
  formatMoney,
  formatTokens,
  goalFingerprint,
  goalFromRow,
  goalTitle,
  idTail,
  kindBreakdown,
  kindBreakdownLabel,
  modelCallFromRow,
  pendingApprovalsOfRun,
  runFingerprint,
  runFromRow,
  runSpend,
  runWaitsOnYou,
  runsOfGoal,
  sameRow,
  spendLabel,
  stepFingerprint,
  stepFromRow,
  stepThought,
  stepsInOrder,
  stepsOfRun,
} from "../../src/apps/nexus/rows";
import {
  approvalKindMeaning,
  approvalKindWord,
  kindCalledAModel,
  runStatusWord,
  stepKindWord,
  waitingWord,
  waitsOnAPerson,
} from "../../src/apps/nexus/words";

// The pure half of the Nexus app. Everything asserted here is a function of a
// row, so nothing below needs a browser, a cluster or React -- which is the
// point of keeping the projections out of the components.

describe("ids and joins", () => {
  it("reads the short id off either spelling", () => {
    expect(idTail("v1:work:run:abc123")).toBe("abc123");
    expect(idTail("abc123")).toBe("abc123");
    expect(idTail("  ")).toBe("");
  });

  it("joins a bare id to a canonical one, which is the case that bites", () => {
    // A row's own id reaches a browser BARE; a relationship field may not.
    // Comparing the two spellings directly is the Accounts app's `self` bug --
    // one wrong comparison left three surfaces permanently empty.
    expect(sameRow("abc123", "v1:work:run:abc123")).toBe(true);
    expect(sameRow("v1:work:run:abc123", "abc123")).toBe(true);
    expect(sameRow("abc123", "def456")).toBe(false);
  });

  it("never matches two blanks, which would join every orphan row to every other", () => {
    expect(sameRow("", "")).toBe(false);
  });

  it("finds a goal's runs across the two spellings", () => {
    const runs = [
      runFromRow({ id: "r1", goalId: "v1:work:goal:g1" }),
      runFromRow({ id: "r2", goalId: "g1" }),
      runFromRow({ id: "r3", goalId: "g2" }),
    ];
    expect(runsOfGoal(runs, "g1").map((r) => r.id)).toEqual(["r1", "r2"]);
  });
});

describe("absent is not zero", () => {
  it("keeps an absent figure absent rather than answering 0", () => {
    // `rowNumber` answers 0 for a missing key. On a run's `spent` that would
    // make this window report "0 model calls" for a run that made three --
    // the single most damaging thing this surface could say, because
    // "it reached no model" is the claim the product makes.
    expect(figure({ tokens: 12 }, "tokens")).toBe(12);
    expect(figure({}, "tokens")).toBeNull();
    expect(figure(null, "tokens")).toBeNull();
    expect(figure({ tokens: "12" }, "tokens")).toBeNull();
    expect(figure({ tokens: Number.NaN }, "tokens")).toBeNull();
  });

  it("reports every spend figure as absent on a run that recorded none", () => {
    const run = runFromRow({ id: "r1" });
    expect(runSpend(run).every((f) => f.value === null)).toBe(true);
    // An ABSENT figure takes the plural: it labels the quantity in general,
    // not a count of one.
    expect(runSpend(run).map(spendLabel)).toEqual(["model calls", "tokens", "cost", "retries"]);
  });

  it("says '1 retry', never '1 retries'", () => {
    const run = runFromRow({ id: "r1", spent: { retries: 1, modelCalls: 1, tokens: 2 } });
    expect(runSpend(run).map(spendLabel)).toEqual(["model call", "tokens", "cost", "retry"]);
  });

  it("keeps an explicit zero, because a run that reached no provider cost nothing", () => {
    const run = runFromRow({ id: "r1", spent: { modelCalls: 0, cost: 0 } });
    const spend = runSpend(run);
    expect(spend.find((f) => f.many === "model calls")?.value).toBe(0);
    expect(formatMoney(spend.find((f) => f.many === "cost")?.value ?? null)).toBe("$0.00");
  });

  it("renders a sub-cent cost rather than rounding it to nothing", () => {
    // "$0.00" beside a model call that happened reads as a rendering fault.
    expect(formatMoney(0.0032)).toBe("$0.0032");
    expect(formatMoney(1.5)).toBe("$1.50");
    expect(formatMoney(null)).toBe("--");
  });

  it("says tokens in the unit a person would", () => {
    expect(formatTokens(940)).toBe("940");
    expect(formatTokens(1240)).toBe("1.2k");
    expect(formatTokens(48000)).toBe("48k");
    expect(formatTokens(2_400_000)).toBe("2.4M");
    expect(formatTokens(null)).toBe("--");
  });
});

describe("the step order", () => {
  it("orders by seq, because the read carries @unbounded and therefore no sort", () => {
    // A timeline drawn in fold order reshuffles itself the moment any step
    // updates -- exactly when somebody is watching it.
    const steps = [
      stepFromRow({ id: "s3", key: "c", seq: 2 }),
      stepFromRow({ id: "s1", key: "a", seq: 0 }),
      stepFromRow({ id: "s2", key: "b", seq: 1 }),
    ];
    expect(stepsInOrder(steps).map((s) => s.key)).toEqual(["a", "b", "c"]);
  });

  it("breaks a tie on the key, so a parallel block does not swap under the reader", () => {
    const steps = [
      stepFromRow({ id: "s2", key: "zeta", seq: 4 }),
      stepFromRow({ id: "s1", key: "alpha", seq: 4 }),
    ];
    expect(stepsInOrder(steps).map((s) => s.key)).toEqual(["alpha", "zeta"]);
  });

  it("selects one run's steps and orders them in the same pass", () => {
    const steps = [
      stepFromRow({ id: "s2", key: "b", seq: 1, runId: "v1:work:run:r1" }),
      stepFromRow({ id: "s9", key: "x", seq: 0, runId: "r2" }),
      stepFromRow({ id: "s1", key: "a", seq: 0, runId: "r1" }),
    ];
    expect(stepsOfRun(steps, "r1").map((s) => s.key)).toEqual(["a", "b"]);
  });
});

describe("which steps thought", () => {
  it("answers reasoning and loop yes, deterministic and decision no", () => {
    expect(kindCalledAModel("reasoning")).toBe(true);
    expect(kindCalledAModel("loop")).toBe(true);
    expect(kindCalledAModel("deterministic")).toBe(false);
    expect(kindCalledAModel("decision")).toBe(false);
    expect(kindCalledAModel("subrun")).toBe(false);
  });

  it("answers NEITHER for an unclassified step", () => {
    // Epic A1 leaves `function` steps with an empty kind. Reading a blank as
    // deterministic would put "no model was called" on a step that may well
    // have called one -- which is the exact claim this surface is here to
    // make, made without evidence.
    expect(kindCalledAModel("")).toBeNull();
    expect(stepKindWord("")).toBe("Unclassified");
    expect(stepThought(stepFromRow({ id: "s", key: "k", seq: 0, kind: "" }))).toBe(false);
  });
});

describe("the kind band", () => {
  const steps = [
    stepFromRow({ id: "1", key: "a", seq: 0, kind: "deterministic" }),
    stepFromRow({ id: "2", key: "b", seq: 1, kind: "deterministic" }),
    stepFromRow({ id: "3", key: "c", seq: 2, kind: "reasoning" }),
    stepFromRow({ id: "4", key: "d", seq: 3, kind: "human" }),
    stepFromRow({ id: "5", key: "e", seq: 4, kind: "" }),
  ];

  it("counts every kind, including the ones at zero", () => {
    // "no steps are waiting on a person" is a reading somebody wants, and an
    // omitted legend row is silence about it.
    const breakdown = kindBreakdown(steps);
    expect(breakdown.total).toBe(5);
    const by = new Map(breakdown.segments.map((s) => [s.kind, s.count]));
    expect(by.get("deterministic")).toBe(2);
    expect(by.get("reasoning")).toBe(1);
    expect(by.get("human")).toBe(1);
    expect(by.get("")).toBe(1);
    expect(by.get("decision")).toBe(0);
    expect(by.get("subrun")).toBe(0);
    expect(breakdown.segments).toHaveLength(7);
  });

  it("reports the thinking and the unclassified separately", () => {
    const breakdown = kindBreakdown(steps);
    expect(breakdown.thought).toBe(1);
    expect(breakdown.unclassified).toBe(1);
  });

  it("emits its segments in a FIXED order, so two runs can be compared by eye", () => {
    const reversed = kindBreakdown([...steps].reverse());
    expect(reversed.segments.map((s) => s.kind)).toEqual(
      kindBreakdown(steps).segments.map((s) => s.kind),
    );
  });

  it("folds an unknown kind into unclassified rather than dropping the step", () => {
    const breakdown = kindBreakdown([
      stepFromRow({ id: "1", key: "a", seq: 0, kind: "somethingNew" }),
    ]);
    expect(breakdown.total).toBe(1);
    expect(breakdown.unclassified).toBe(1);
  });

  it("shares sum to one, so the band can never show a gap that reads as a fault", () => {
    const total = kindBreakdown(steps).segments.reduce((sum, s) => sum + s.share, 0);
    expect(total).toBeCloseTo(1, 10);
  });

  it("speaks itself for a reader who cannot see it", () => {
    // A bar a screen reader cannot read is a bar that excluded somebody, and
    // the picture's whole content is proportion.
    expect(kindBreakdownLabel(kindBreakdown(steps))).toBe(
      "5 steps: 2 deterministic, 1 reasoning, 1 human, 1 unclassified.",
    );
    expect(kindBreakdownLabel(kindBreakdown([]))).toBe("No steps yet.");
  });
});

describe("the arrival cue's fingerprints", () => {
  it("IGNORES the heartbeat, which is this app's sharpest case of the rule", () => {
    // A running run writes `heartbeatAt` at every step boundary and
    // broadcasts the whole row. Naming it would ring the row somebody is
    // already watching, hardest, for the whole duration of the run.
    const before = runFromRow({ id: "r1", status: "running", heartbeatAt: "2026-09-01T09:05:00Z" });
    const after = runFromRow({ id: "r1", status: "running", heartbeatAt: "2026-09-01T09:05:15Z" });
    expect(runFingerprint(after)).toBe(runFingerprint(before));
  });

  it("IGNORES the counters, which must re-render live and must not ring", () => {
    const before = runFromRow({ id: "r1", status: "running", spent: { tokens: 100 } });
    const after = runFromRow({ id: "r1", status: "running", spent: { tokens: 8000 } });
    expect(runFingerprint(after)).toBe(runFingerprint(before));
  });

  it("RINGS on a state change, on parking, and on a cancel request", () => {
    const running = runFromRow({ id: "r1", status: "running" });
    expect(runFingerprint(runFromRow({ id: "r1", status: "succeeded" }))).not.toBe(
      runFingerprint(running),
    );
    expect(
      runFingerprint(
        runFromRow({ id: "r1", status: "waiting", waitingOn: { kind: "approval" } }),
      ),
    ).not.toBe(runFingerprint(running));
    expect(
      runFingerprint(runFromRow({ id: "r1", status: "running", cancelRequested: true })),
    ).not.toBe(runFingerprint(running));
  });

  it("rings a step on a state change, a classification and a retry", () => {
    const base = stepFromRow({ id: "s1", key: "a", seq: 0, status: "running" });
    expect(
      stepFingerprint(stepFromRow({ id: "s1", key: "a", seq: 0, status: "done" })),
    ).not.toBe(stepFingerprint(base));
    expect(
      stepFingerprint(
        stepFromRow({ id: "s1", key: "a", seq: 0, status: "running", attempt: 2 }),
      ),
    ).not.toBe(stepFingerprint(base));
  });

  it("rings a goal on a restatement and on a status flip", () => {
    const base = goalFromRow({ id: "g1", statement: "Ship it", status: "open" });
    expect(goalFingerprint(goalFromRow({ id: "g1", statement: "Ship it now", status: "open" })))
      .not.toBe(goalFingerprint(base));
    expect(goalFingerprint(goalFromRow({ id: "g1", statement: "Ship it", status: "closed" })))
      .not.toBe(goalFingerprint(base));
  });
});

describe("run readings", () => {
  it("knows when a run is stuck on a PERSON rather than on a timer", () => {
    const onYou = runFromRow({
      id: "r1",
      status: "waiting",
      waitingOn: { kind: "approval", subject: "sendInvoice" },
    });
    const onATimer = runFromRow({ id: "r2", status: "waiting", waitingOn: { kind: "timer" } });
    expect(runWaitsOnYou(onYou)).toBe(true);
    expect(runWaitsOnYou(onATimer)).toBe(false);
  });

  it("calls a stopped run stopped and a lost run lost, never failed", () => {
    // The distinction is WHO DECIDED. Collapsing them into "failed" reports a
    // person's own click back to them as a fault, and sends somebody to debug
    // a run whose node simply went away.
    expect(runStatusWord("cancelled")).toBe("Stopped");
    expect(runStatusWord("abandoned")).toBe("Lost");
    expect(runStatusWord("failed")).toBe("Failed");
  });

  it("never renders a blank name", () => {
    expect(goalTitle(goalFromRow({ id: "v1:work:goal:ab12", statement: "  " }))).toBe(
      "Untitled goal (ab12)",
    );
  });

  it("says what a run parked by the failure path is actually doing", () => {
    // THE BUG: the three kinds the failure path writes -- retry, replan,
    // repair -- had no words here, so all three fell to the default and read
    // a bare "Waiting". Somebody watched one of those believing work was in
    // flight, when what had happened was a step failing, a classifier
    // deciding, and the system queueing its own next move. Each says so now.
    expect(waitingWord("retry")).toBe("A step failed and it is going to try that step again");
    expect(waitingWord("replan")).toBe(
      "A step failed and the rest of the plan is being worked out again",
    );
    expect(waitingWord("repair")).toBe(
      "A step did something other than what it promised, and it is being redone",
    );

    // NONE OF THE THREE IS A QUESTION FOR ANYBODY, so none of them may show
    // up in the "waiting for you" count. That count is the app's one urgency
    // signal; padding it with waits the system serves itself teaches people
    // to ignore it.
    for (const kind of ["retry", "replan", "repair"]) {
      expect(waitsOnAPerson(kind)).toBe(false);
      expect(runWaitsOnYou(runFromRow({ id: "r", status: "waiting", waitingOn: { kind } }))).toBe(
        false,
      );
    }
    expect(waitsOnAPerson("approval")).toBe(true);
    expect(waitsOnAPerson("feedback")).toBe(true);
  });

  it("asks about money in words that are true of both ways money runs out", () => {
    // Two things raise a `budget` approval: the run crossing a ceiling its
    // goal declared, where approving raises it, and the balance behind paid
    // calls running out, where approving raises nothing and somebody has to
    // top it up first. The old sentence promised a button that does not exist
    // in the second case.
    const meaning = approvalKindMeaning("budget");
    expect(meaning).toContain("paid model calls");
    expect(meaning).not.toContain("Approving lets it carry on spending");
    expect(approvalKindWord("budget")).toBe("Budget");
  });
});

describe("approvals", () => {
  it("keeps only options that can actually be sent", () => {
    // An option with no value cannot be sent, so it is dropped rather than
    // rendered as a button that produces a refusal. One with a value and no
    // label falls back to the value: hiding it would leave somebody with a
    // question they cannot answer.
    expect(
      approvalOptions([
        { label: "Yes", value: "yes" },
        { label: "No label" },
        { value: "bare" },
        "not an object",
        null,
      ]),
    ).toEqual([
      { label: "Yes", value: "yes" },
      { label: "bare", value: "bare" },
    ]);
    expect(approvalOptions(undefined)).toEqual([]);
  });

  it("leads with the question, then the subject, then the step", () => {
    expect(
      approvalSubjectLine(approvalFromRow({ id: "a", question: "Which account?" })),
    ).toBe("Which account?");
    expect(
      approvalSubjectLine(
        approvalFromRow({ id: "a", subject: { summary: "Email the invoice to Acme" } }),
      ),
    ).toBe("Email the invoice to Acme");
    expect(approvalSubjectLine(approvalFromRow({ id: "a", stepKey: "sendInvoice" }))).toBe(
      "Step sendInvoice",
    );
  });

  it("finds the pending approvals of one run and ignores the decided ones", () => {
    const approvals = [
      approvalFromRow({ id: "a1", runId: "v1:work:run:r1", decision: "" }),
      approvalFromRow({ id: "a2", runId: "r1", decision: "approved" }),
      approvalFromRow({ id: "a3", runId: "r2", decision: "" }),
    ];
    expect(pendingApprovalsOfRun(approvals, "r1").map((a) => a.id)).toEqual(["a1"]);
  });
});

describe("the postcondition has three answers", () => {
  it("keeps 'none declared' distinct from 'did not pass'", () => {
    // Epic A1 declares no postcondition on any step. Reading an absent one as
    // false would mark every run in the cluster failed on the strength of a
    // field nobody wrote.
    expect(stepFromRow({ id: "s", key: "k", seq: 0 }).postconditionPassed).toBeNull();
    expect(
      stepFromRow({ id: "s", key: "k", seq: 0, postcondition: { passed: false } })
        .postconditionPassed,
    ).toBe(false);
    expect(
      stepFromRow({ id: "s", key: "k", seq: 0, postcondition: { passed: true } })
        .postconditionPassed,
    ).toBe(true);
  });
});

describe("which door answered a step", () => {
  const call = (over: Record<string, unknown>) =>
    modelCallFromRow({
      runId: "r1",
      stepKey: "classify",
      provider: "anthropic",
      model: "claude-sonnet-4",
      served: "live",
      createdAt: "2026-09-01T09:00:00Z",
      ...over,
    });

  it("folds a step's calls into one reading rather than one line each", () => {
    // The step row is a ROW: the spine works by weight, and a row that is
    // sometimes one line tall and sometimes four takes that away from every
    // other row on the page.
    const decisions = decisionsByStep([
      call({ id: "m1", cost: 0.002, inputTokens: 100, outputTokens: 40 }),
      call({ id: "m2", cost: 0.003, inputTokens: 200, outputTokens: 60, model: "claude-opus-5", createdAt: "2026-09-01T09:01:00Z" }),
    ]);
    expect(decisions.size).toBe(1);
    const decision = decisions.get("classify");
    expect(decision?.calls).toBe(2);
    expect(decision?.cost).toBeCloseTo(0.005, 10);
    expect(decision?.tokens).toBe(400);
    // The LAST model in time, not the first the read happened to answer with.
    expect(decision?.model).toBe("claude-opus-5");
  });

  it("orders by TIME, because @unbounded excludes sort and the read arrives folded", () => {
    const decisions = decisionsByStep([
      call({ id: "m2", model: "second", createdAt: "2026-09-01T09:05:00Z" }),
      call({ id: "m1", model: "first", createdAt: "2026-09-01T09:01:00Z" }),
    ]);
    expect(decisions.get("classify")?.model).toBe("second");
  });

  it("reports the MOST EXPENSIVE door a step went through, with the model that went through it", () => {
    // A step that made two fleet calls and one vendor call WAS billed. Naming
    // it "your fleet" would print money under a line saying nothing was
    // billed -- the row contradicting itself, in the one place this surface
    // exists to be trusted.
    const decisions = decisionsByStep([
      call({ id: "m1", served: "local", provider: "", model: "qwen3:8b" }),
      call({ id: "m2", served: "live", provider: "anthropic", model: "claude-sonnet-4", cost: 0.004, createdAt: "2026-09-01T09:01:00Z" }),
      call({ id: "m3", served: "local", provider: "", model: "qwen3:8b", createdAt: "2026-09-01T09:02:00Z" }),
    ]);
    const decision = decisions.get("classify");
    expect(decision?.served).toBe("live");
    expect(decision?.provider).toBe("anthropic");
    expect(decision?.model).toBe("claude-sonnet-4");
    expect(decisionLine(decision!)).toBe("anthropic · claude-sonnet-4 · $0.0040 · 3 calls");
  });

  it("drops a call that names no step rather than attributing it to one", () => {
    // The journal panel below still lists it. Guessing which step it belonged
    // to would put a cost on a row that did not incur it.
    expect(decisionsByStep([call({ id: "m1", stepKey: "" })]).size).toBe(0);
  });

  it("says a vendor cost was not reported, rather than rendering it as free", () => {
    // The visible difference between a fleet line and a vendor line is the
    // money, so a vendor line with the money silently missing is a billed call
    // dressed as a free one. Absent is not zero, here as everywhere.
    const decision = decisionsByStep([call({ id: "m1" })]).get("classify");
    expect(decision?.cost).toBeNull();
    expect(decisionLine(decision!)).toBe("anthropic · claude-sonnet-4 · cost not reported");
  });

  it("puts no money on a fleet line at all", () => {
    const decision = decisionsByStep([
      call({ id: "m1", served: "local", provider: "", model: "qwen3:8b" }),
    ]).get("classify");
    expect(decisionLine(decision!)).toBe("your fleet · qwen3:8b");
  });

  it("counts a replay's ANSWERS, because 'no calls made · 3 calls' argues with itself", () => {
    const decision = decisionsByStep([
      call({ id: "m1", served: "journal", model: "claude-haiku" }),
      call({ id: "m2", served: "journal", model: "claude-haiku", createdAt: "2026-09-01T09:01:00Z" }),
    ]).get("classify");
    expect(decisionLine(decision!)).toBe("replayed · claude-haiku, no calls made · 2 answers");
  });

  it("keeps an unknown door VISIBLE rather than hiding it behind a free one", () => {
    // A door this build has no word for might be a billed one. Reporting the
    // step as "your fleet" would be this window claiming nothing was billed on
    // the strength of a value it does not understand.
    const decision = decisionsByStep([
      call({ id: "m1", served: "local", provider: "", model: "qwen3:8b" }),
      call({ id: "m2", served: "somethingNew", createdAt: "2026-09-01T09:01:00Z" }),
    ]).get("classify");
    expect(decision?.served).toBe("somethingNew");
    // ...and still below a vendor call, which is the claim that asserts most.
    const billed = decisionsByStep([
      call({ id: "m1", served: "somethingNew" }),
      call({ id: "m2", served: "live", cost: 0.004, createdAt: "2026-09-01T09:01:00Z" }),
    ]).get("classify");
    expect(billed?.served).toBe("live");
  });

  it("names an unknown door by its own value rather than guessing the commonest", () => {
    // Guessing would put a confident claim about money on a row nothing here
    // understands.
    const decision = decisionsByStep([call({ id: "m1", served: "somethingNew" })]).get("classify");
    expect(decisionLine(decision!)).toBe("somethingNew · claude-sonnet-4");
  });
});
