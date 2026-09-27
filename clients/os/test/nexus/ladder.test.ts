import { describe, expect, it } from "vitest";

import {
  evidenceLine,
  ladderPolicyFromRow,
  ladderWord,
  nextRungSentence,
  procedureBand,
  procedureFromRow,
  procedureTitle,
  promotionSubject,
  promotionTargetWords,
  rungStates,
  stepArgs,
  tokensOf,
  zeroCountWord,
  type ArgNode,
} from "../../src/apps/nexus/ladder";
import { approvalFromRow } from "../../src/apps/nexus/rows";
import { approvalRow, ladderPolicyRow, procedureRow } from "./harness";

// The ladder's readings, as pure functions of a row (epic memql#5408, #5412).
//
// What a list row says and what the page's next-rung sentence says are the
// readings a person plans around, so they are pinned here without a browser --
// above all the two that go wrong silently: a threshold made up where the
// cluster published none, and a number that is really a percentage.

const POLICY = ladderPolicyFromRow(ladderPolicyRow());

function proc(over: Record<string, unknown> = {}) {
  return procedureFromRow(procedureRow({ id: "p1", ...over }));
}

describe("the policy row", () => {
  it("is null when the cluster published none", () => {
    expect(ladderPolicyFromRow(null)).toBeNull();
    expect(ladderPolicyFromRow(undefined)).toBeNull();
  });

  it("reads a zero or a missing value as unknown, never as zero", () => {
    const policy = ladderPolicyFromRow({ id: "x", shadowMatches: 0, distinctBindings: 2 });
    expect(policy?.shadowMatches).toBeNull();
    expect(policy?.canaryMatches).toBeNull();
    expect(policy?.distinctBindings).toBe(2);
  });
});

describe("where it stands", () => {
  it("passes the rungs below, stands on one, and has not reached the rest", () => {
    expect(rungStates("canary").map((s) => s.state)).toEqual(["passed", "passed", "current", "ahead"]);
  });

  it("stands on no climbing rung when retired or off the ladder", () => {
    expect(rungStates("retired").every((s) => s.state === "ahead")).toBe(true);
    expect(rungStates("").every((s) => s.state === "ahead")).toBe(true);
  });

  it("says what a zero means from where the procedure stands", () => {
    // Below the rung a count is earned on, or on it: nothing earned yet.
    expect(zeroCountWord("candidate", "canary")).toBe("none yet");
    expect(zeroCountWord("canary", "canary")).toBe("none yet");
    // Past it, the rung was passed and the zero is a reset counter: say nothing.
    expect(zeroCountWord("trusted", "canary")).toBeNull();
    // Off the ladder nothing more will be earned.
    expect(zeroCountWord("retired", "shadow")).toBe("none");
  });

  it("names an absent rung for what it is rather than guessing the bottom one", () => {
    expect(ladderWord("")).toBe("Off the ladder");
    expect(ladderWord("sideways")).toBe("sideways");
  });
});

describe("the next rung, from the policy", () => {
  it("counts what shadow still needs, naming the parameter by its goal input", () => {
    expect(nextRungSentence(proc(), POLICY)).toBe(
      "It needs 2 more matches in a row and a second binding for month; then promotion to canary is put to you.",
    );
  });

  it("says a promotion is waiting when one is open", () => {
    expect(nextRungSentence(proc({ promotionApprovalId: "a1" }), POLICY)).toMatch(/^Promotion to canary is waiting for your decision/);
  });

  it("says nothing numeric when the cluster published no policy", () => {
    const text = nextRungSentence(proc(), null);
    expect(text).toBe("How far it has to go is not shown: this cluster has not published its ladder values.");
    expect(text).not.toMatch(/\d/);
  });

  it("counts a canary's clean replays toward trust", () => {
    expect(nextRungSentence(proc({ ladder: "canary", canaryMatches: 4 }), POLICY)).toBe(
      "1 more clean replay in a row and it is trusted to run without a model. Nobody is asked again.",
    );
  });
});

describe("the one line a list row says", () => {
  it("counts matches against m while they are short", () => {
    expect(evidenceLine(proc(), POLICY)).toEqual({ text: "3 of 5 matches beside the app", tone: "quiet" });
  });

  it("names the binding still missing once the matches are met", () => {
    expect(evidenceLine(proc({ shadowMatches: 5 }), POLICY).text).toBe("Needs a second binding");
  });

  it("puts the promotion first and in the waiting tone", () => {
    expect(evidenceLine(proc({ promotionApprovalId: "a1" }), POLICY)).toEqual({
      text: "Promotion waiting for you",
      tone: "waiting",
    });
    expect(procedureBand(proc({ promotionApprovalId: "a1" }))).toBe(0);
  });

  it("drops the threshold rather than inventing one", () => {
    expect(evidenceLine(proc(), null).text).toBe("3 matches beside the app");
    expect(evidenceLine(proc({ ladder: "canary", canaryMatches: 2 }), null).text).toBe("2 clean replays");
  });

  it("says a failing procedure is failing, in the falling tone", () => {
    expect(evidenceLine(proc({ ladder: "trusted", failures: 1 }), POLICY)).toEqual({
      text: "1 failed replay in a row",
      tone: "fall",
    });
  });

  it("never prints a percentage", () => {
    for (const ladder of ["candidate", "shadow", "canary", "trusted", "retired", ""]) {
      expect(evidenceLine(proc({ ladder, reliability: 0.62 }), POLICY).text).not.toMatch(/%/);
    }
  });
});

describe("a step, written back", () => {
  const lit = (value: string, litType = ""): ArgNode => ({
    kind: "lit",
    keys: [],
    kids: [],
    form: "",
    lit: value,
    litType,
    holeId: "",
    holeType: "",
  });
  const hole = (id: string): ArgNode => ({ ...lit(""), kind: "hole", holeId: id });
  const arr = (form: string, kids: ArgNode[]): ArgNode => ({ ...lit(""), kind: "array", form, kids });

  it("joins a command line by spaces, quoting only what has to be", () => {
    expect(tokensOf(arr("argv", [lit("echo"), lit("two words"), hole("h1")]))).toEqual([
      { kind: "text", text: "echo 'two words' " },
      { kind: "hole", holeId: "h1" },
    ]);
  });

  it("joins a path by slashes and keeps a leading root", () => {
    expect(tokensOf(arr("rootedPath", [lit("tmp"), lit("out.csv")]))).toEqual([
      { kind: "text", text: "/tmp/out.csv" },
    ]);
  });

  it("keeps an empty path segment empty rather than quoting it", () => {
    // "a//b" is not "a/b", and a quoted '' in the middle of a path is a
    // command-line spelling that does not belong there.
    expect(tokensOf(arr("path", [lit("https:"), lit(""), lit("host"), hole("h1")]))).toEqual([
      { kind: "text", text: "https://host/" },
      { kind: "hole", holeId: "h1" },
    ]);
  });

  it("writes a JSON document back as compact JSON", () => {
    const doc: ArgNode = {
      ...lit(""),
      kind: "object",
      keys: ["n", "s"],
      kids: [lit("1", "number"), lit("x", "string")],
    };
    expect(tokensOf(doc)).toEqual([{ kind: "text", text: '{n: 1, s: "x"}' }]);
  });

  it("leads with the argument that says what the step does", () => {
    const procedure = proc();
    const args = stepArgs(procedure.steps[1]?.args ?? null);
    expect(args.primary).toEqual([
      { kind: "text", text: "reports/" },
      { kind: "hole", holeId: "s1.path.1" },
    ]);
    expect(args.rest.map((r) => r.key)).toEqual(["content"]);
  });
});

describe("reading a row", () => {
  it("reads a procedure object that arrived as a JSON string", () => {
    const raw = procedureRow({ id: "p1" });
    const p = procedureFromRow({ ...raw, procedure: JSON.stringify(raw["procedure"]) });
    expect(procedureTitle(p)).toBe("Reconcile last month's ledger against the bank export");
    expect(p.steps).toHaveLength(2);
  });

  it("keeps an absent reliability absent", () => {
    expect(procedureFromRow({ id: "p1" }).reliability).toBeNull();
  });
});

describe("the promotion's subject", () => {
  it("is read only off a promotion", () => {
    expect(promotionSubject(approvalFromRow(approvalRow({ id: "a1" })))).toBeNull();
  });

  it("keeps the counts per parameter and the provenance", () => {
    const subject = promotionSubject(
      approvalFromRow(
        approvalRow({
          id: "a1",
          kind: "procedurePromotion",
          subject: {
            constructId: "p1",
            procedureHash: "sha256:v1",
            shadowMatches: 5,
            distinctBindings: { b: 3, a: 2 },
            recordedFrom: { app: "codex", sessionIds: ["s1"] },
          },
        }),
      ),
    );
    expect(subject?.distinctBindings).toEqual([
      ["a", 2],
      ["b", 3],
    ]);
    expect(subject?.shadowMatches).toBe(5);
    expect(subject?.recordedFrom.app).toBe("codex");
  });

  function subjectOf(over: Record<string, unknown>) {
    return promotionSubject(
      approvalFromRow(
        approvalRow({
          id: "a1",
          kind: "procedurePromotion",
          subject: { constructId: "p1", procedureHash: "sha256:v1", shadowMatches: 5, ...over },
        }),
      ),
    );
  }

  it("reads where it would run and whether its matches were dry", () => {
    const subject = subjectOf({ target: "workbench", dryEvidence: true });
    expect(subject?.target).toBe("workbench");
    expect(subject?.dryEvidence).toBe(true);
    expect(subjectOf({ target: "machine", dryEvidence: false })?.dryEvidence).toBe(false);
  });

  it("reads both keys as absent when the engine did not send them", () => {
    const subject = subjectOf({});
    expect(subject?.target).toBe("");
    expect(subject?.dryEvidence).toBe(false);
    // Only a real boolean says the evidence was dry; a string is not one.
    expect(subjectOf({ dryEvidence: "true" })?.dryEvidence).toBe(false);
  });
});

describe("where a promotion would run", () => {
  it("names the two places a replay runs, in the card's own words", () => {
    expect(promotionTargetWords("workbench")).toBe("In the workbench, a sandbox in your cluster");
    expect(promotionTargetWords("machine")).toBe("On your machine");
  });

  it("says nothing for an absent target, and a value it has no word for as itself", () => {
    expect(promotionTargetWords("")).toBe("");
    expect(promotionTargetWords("   ")).toBe("");
    expect(promotionTargetWords("gpu-pool")).toBe("gpu-pool");
  });
});
