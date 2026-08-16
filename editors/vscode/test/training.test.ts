// Training-state presentation (memql#3761).
//
// Two acceptance criteria here are the interesting ones, because both are
// normally checked by looking at a screen:
//
//   "syntax colouring is unchanged with decorations on -- asserted, not
//    eyeballed"        -> a decoration must set no `color` and no
//                         `textDecoration`. Those are the only two keys that
//                         can alter a token's rendering.
//   "three icons, distinguishable without relying on colour alone"
//                      -> the three GEOMETRIES must differ with every colour
//                         stripped out.
//
// The third is `unknown`, which must render as nothing at all -- and which is
// asserted from several directions because every one of its failures looks like
// the feature working.

import test from "node:test";
import assert from "node:assert/strict";

import {
  COMMAND_PROMOTE,
  TRAINING_STATES,
  countStates,
  gutterMarkFor,
  parseTrainingConstructs,
  statusBarText,
  statusBarTooltip,
  trainingLensPlans,
  trainingListEntries,
  type TrainingConstruct,
  type TrainingState,
} from "../src/state/training.js";
import { gutterShape, gutterSvg, gutterIconUri, markDecoration } from "../src/constructs/gutterIcons.js";

function construct(state: TrainingState, name = "spaceParticipants"): TrainingConstruct {
  return {
    kind: "query",
    name,
    signatureRange: { start: { line: 3, character: 0 }, end: { line: 3, character: 34 } },
    state,
  };
}

function wire(entries: unknown[]): unknown {
  return { constructs: entries };
}

// -----------------------------------------------------------------------------
// the gutter's three answers
// -----------------------------------------------------------------------------

test("the gutter has three marks for five states, and trained shares with seeded", () => {
  // The gutter answers one question -- does what I am looking at match what
  // runs -- and that has three answers. `trained` and `seeded` are the same
  // answer to it; they differ in ACTIONS, which is the lens's job.
  assert.equal(gutterMarkFor("untrained"), "untrained");
  assert.equal(gutterMarkFor("drifted"), "drifted");
  assert.equal(gutterMarkFor("trained"), "live");
  assert.equal(gutterMarkFor("seeded"), "live");
});

test("unknown gets NO mark -- not a grey one", () => {
  // A mark meaning "we do not know" is indistinguishable at 16 pixels from one
  // meaning "something is wrong here", and a disconnection must not look like a
  // problem with the file.
  assert.equal(gutterMarkFor("unknown"), undefined);
});

test("every state is handled -- no state falls through to undefined by accident", () => {
  for (const state of TRAINING_STATES) {
    const mark = gutterMarkFor(state);
    assert.equal(mark === undefined, state === "unknown", state);
  }
});

// -----------------------------------------------------------------------------
// distinguishable WITHOUT COLOUR
// -----------------------------------------------------------------------------

test("the three glyphs differ in geometry, with every colour stripped", () => {
  // The acceptance criterion, as something a machine checks. If someone later
  // makes all three a circle and separates them by fill colour alone, this
  // fails.
  const shapes = ["untrained", "drifted", "live"].map((m) =>
    gutterShape(m as "untrained" | "drifted" | "live"),
  );
  assert.equal(new Set(shapes).size, 3, `geometries collapsed: ${shapes.join(" | ")}`);
});

test("the hollow ring is genuinely hollow, which is its whole distinction from live", () => {
  assert.match(gutterShape("untrained"), /fill="none"/);
  assert.doesNotMatch(gutterShape("live"), /fill="none"/);
});

test("the svg carries no unsubstituted colour placeholder", () => {
  for (const mark of ["untrained", "drifted", "live"] as const) {
    const svg = gutterSvg(mark);
    assert.doesNotMatch(svg, /COLOR/, mark);
    assert.match(svg, /^<svg /, mark);
  }
});

test("the data uri is base64, so a colour literal's # cannot truncate it", () => {
  // Percent-encoding an SVG leaves `#3f9142` in the URI, and a parser treating
  // `#` as a fragment separator cuts the document at the first colour.
  const uri = gutterIconUri("live");
  assert.match(uri, /^data:image\/svg\+xml;base64,/);
  assert.doesNotMatch(uri, /#/);
  const decoded = Buffer.from(uri.split(",")[1] ?? "", "base64").toString("utf8");
  assert.equal(decoded, gutterSvg("live"));
});

// -----------------------------------------------------------------------------
// does not fight syntax colouring
// -----------------------------------------------------------------------------

test("no decoration sets color or textDecoration", () => {
  // The mechanical meaning of "does not fight syntax colouring". Both keys are
  // checked: `textDecoration` takes an arbitrary CSS declaration and can carry
  // a colour, so asserting on `color` alone leaves the loophole open.
  for (const mark of ["untrained", "drifted", "live"] as const) {
    const decoration = markDecoration(mark) as unknown as Record<string, unknown>;
    assert.equal("color" in decoration, false, `${mark} sets color`);
    assert.equal("textDecoration" in decoration, false, `${mark} sets textDecoration`);
  }
});

test("live decorates the gutter only, leaving the text area untouched", () => {
  // In a healthy file every construct is live. Washing every signature would be
  // a permanent tint over the code conveying nothing the gutter does not.
  const live = markDecoration("live") as unknown as Record<string, unknown>;
  assert.equal(live.backgroundColor, undefined);
  assert.equal(live.border, undefined);
  assert.ok(typeof live.gutterIconPath === "string");
});

test("the attention states do decorate the range, subtly", () => {
  for (const mark of ["untrained", "drifted"] as const) {
    const d = markDecoration(mark) as unknown as Record<string, string>;
    assert.ok(d.backgroundColor !== undefined, mark);
    // Alpha-blended, so it sits under the syntax colours rather than replacing
    // them. An 8-digit hex is the assertion that an alpha channel is present.
    assert.match(d.backgroundColor, /^#[0-9a-f]{8}$/i, mark);
    assert.match(d.border ?? "", /dotted/, mark);
  }
});

// -----------------------------------------------------------------------------
// the lens
// -----------------------------------------------------------------------------

test("each state gets its own label", () => {
  const plans = trainingLensPlans([
    construct("untrained", "a"),
    construct("drifted", "b"),
    construct("trained", "c"),
    construct("seeded", "d"),
  ]);
  assert.deepEqual(plans.map((p) => p.label), ["untrained", "drifted", "trained", "seeded"]);
});

test("unknown produces NO plan at all -- not an empty one", () => {
  // The caller renders what it gets, so absence here is absence on screen.
  assert.deepEqual(trainingLensPlans([construct("unknown")]), []);
});

test("a mixed file shows all three simultaneously, in document order", () => {
  // The acceptance criterion verbatim.
  const plans = trainingLensPlans([
    construct("trained", "a"),
    construct("untrained", "b"),
    construct("drifted", "c"),
  ]);
  assert.deepEqual(plans.map((p) => `${p.construct.name}:${p.label}`), [
    "a:trained",
    "b:untrained",
    "c:drifted",
  ]);
});

test("the actions are OFF by default, because #3763 registers the commands", () => {
  // A lens posting to an unregistered command fails with "command not found",
  // and a click that does nothing teaches a developer the extension is broken.
  for (const state of ["untrained", "drifted", "trained", "seeded", "staged"] as const) {
    assert.deepEqual(trainingLensPlans([construct(state)])[0]?.actions, [], state);
  }
});

test("with actions on, each state offers exactly what the design's table says", () => {
  const of = (state: TrainingState): string[] =>
    trainingLensPlans([construct(state)], { offerActions: true })[0]?.actions.map((a) => a.title) ??
    [];

  // The order is the ESCALATION: a session that ends, a private one that does
  // not, and one everybody gets.
  assert.deepEqual(of("untrained"), ["Dry-run", "Try in session", "Stage", "Promote"]);
  assert.deepEqual(of("drifted"), [
    "Dry-run",
    "Try in session",
    "Stage",
    "Promote (updates the trained version)",
  ]);
  assert.deepEqual(of("trained"), ["Demote"]);
  // Not a disabled control: absent. A seeded construct needs a rollout, which
  // is not something this editor can offer.
  assert.deepEqual(of("seeded"), []);
  // Staged is the only state with a Train, and Train is COMMAND_PROMOTE under a
  // title that names the consequence -- on the wire it IS a promote.
  assert.deepEqual(of("staged"), [
    "Re-stage",
    "Train (make it live for everyone)",
    "Demote",
  ]);
});

test("staged trains through the promote command, not a command of its own", () => {
  // The wire has one verb for it, so the editor has one command for it. A second
  // command would be a second name for one operation, and the two would then
  // have to be kept doing the same thing.
  const [plan] = trainingLensPlans([construct("staged")], { offerActions: true });
  const train = plan?.actions.find((a) => a.title.startsWith("Train"));
  assert.equal(train?.command, COMMAND_PROMOTE);
});

test("staged says who can call it, which is the whole of what makes it not trained", () => {
  const [plan] = trainingLensPlans([construct("staged")]);
  assert.match(plan?.detail ?? "", /callable by you and by nobody else/);
  assert.match(plan?.detail ?? "", /Train it/);
});

test("drifted names what promoting will do, because it is not the same act", () => {
  const [plan] = trainingLensPlans([construct("drifted")], { offerActions: true });
  assert.match(plan?.actions.at(-1)?.title ?? "", /updates the trained version/);
});

test("seeded explains why there is nothing to do", () => {
  // Its whole content is the absence of an action, so the sentence has to carry
  // the reason or the developer reads the gap as a bug.
  const [plan] = trainingLensPlans([construct("seeded")]);
  assert.match(plan?.detail ?? "", /rollout/);
  assert.match(plan?.detail ?? "", /nothing here to demote/);
});

test("untrained says the thing this whole surface exists to say", () => {
  const [plan] = trainingLensPlans([construct("untrained")]);
  assert.match(plan?.detail ?? "", /Saving the file does not change that/);
});

// -----------------------------------------------------------------------------
// the status bar
// -----------------------------------------------------------------------------

test("the status bar reports only what needs attention", () => {
  const counts = countStates([
    construct("untrained", "a"),
    construct("untrained", "b"),
    construct("untrained", "c"),
    construct("drifted", "d"),
    construct("trained", "e"),
    construct("seeded", "f"),
    construct("staged", "g"),
  ]);
  assert.deepEqual(counts, { untrained: 3, drifted: 1, trained: 1, seeded: 1, staged: 1 });
  // The design's example, exactly.
  assert.equal(statusBarText(counts), "3 untrained · 1 drifted");
});

test("an all-trained file says nothing", () => {
  // An item that is always present saying "12 trained" is one a developer stops
  // reading, which takes the warning with it.
  assert.equal(statusBarText(countStates([construct("trained"), construct("seeded")])), "");
});

test("a staged construct needs no attention, so the status bar stays silent about it", () => {
  // The bar exists to make "I saved but did not promote" impossible to miss. A
  // staged construct HAS been acted on -- it is durable on the cluster -- so
  // counting it here would put a permanent number in front of a developer
  // iterating on one, which is how the warning stops being read.
  assert.equal(statusBarText(countStates([construct("staged")])), "");
});

test("a zero count is omitted rather than shown as `0 drifted`", () => {
  assert.equal(statusBarText(countStates([construct("untrained")])), "1 untrained");
  assert.equal(statusBarText(countStates([construct("drifted")])), "1 drifted");
});

test("unknown counts toward nothing", () => {
  const counts = countStates([construct("unknown", "a"), construct("unknown", "b")]);
  assert.deepEqual(counts, { untrained: 0, drifted: 0, trained: 0, seeded: 0, staged: 0 });
  assert.equal(statusBarText(counts), "");
});

test("the tooltip is where saving-is-not-promoting is said in words", () => {
  const tip = statusBarTooltip(countStates([construct("untrained"), construct("drifted", "b")]));
  assert.match(tip, /Saving this file does not promote anything/);
});

test("no tooltip when there is nothing to report", () => {
  assert.equal(statusBarTooltip(countStates([construct("trained")])), "");
});

// -----------------------------------------------------------------------------
// decoding an untrusted reply
// -----------------------------------------------------------------------------

test("a well-formed reply parses", () => {
  const parsed = parseTrainingConstructs(
    wire([
      {
        kind: "query",
        name: "a",
        signatureRange: { start: { line: 1, character: 0 }, end: { line: 1, character: 9 } },
        state: "drifted",
      },
    ]),
  );
  assert.equal(parsed.length, 1);
  assert.equal(parsed[0]?.state, "drifted");
  assert.equal(parsed[0]?.signatureRange.end.character, 9);
});

test("a state this build has never heard of degrades to unknown, so it renders as nothing", () => {
  // THE SEAM. Five state names cross the wire as a bare string, and this is the
  // failure mode: silent, because unknown renders as nothing. Degrading rather
  // than dropping keeps the construct in the list while showing no training UI
  // for it.
  const parsed = parseTrainingConstructs(
    wire([
      {
        kind: "query",
        name: "a",
        signatureRange: { start: { line: 0, character: 0 }, end: { line: 0, character: 1 } },
        state: "quarantined",
      },
    ]),
  );
  assert.equal(parsed.length, 1);
  assert.equal(parsed[0]?.state, "unknown");
  assert.deepEqual(trainingLensPlans(parsed), []);
  assert.equal(gutterMarkFor(parsed[0]!.state), undefined);
});

test("a missing state is unknown too, which is what an older server sends", () => {
  const parsed = parseTrainingConstructs(
    wire([
      {
        kind: "query",
        name: "a",
        signatureRange: { start: { line: 0, character: 0 }, end: { line: 0, character: 1 } },
      },
    ]),
  );
  assert.equal(parsed[0]?.state, "unknown");
});

test("an entry with no usable range is dropped -- there is nowhere to draw it", () => {
  const parsed = parseTrainingConstructs(
    wire([
      { kind: "query", name: "a" },
      { kind: "query", name: "b", signatureRange: { start: { line: 0 }, end: {} } },
      { kind: "query", name: "", signatureRange: { start: { line: 0, character: 0 }, end: { line: 0, character: 1 } } },
    ]),
  );
  assert.deepEqual(parsed, []);
});

test("a malformed envelope is an empty list, never a throw", () => {
  // A thrown error inside provideCodeLenses reaches the user as a popup on a
  // document they are simply still typing.
  for (const raw of [null, undefined, 42, "nope", {}, { constructs: null }, { constructs: 7 }]) {
    assert.deepEqual(parseTrainingConstructs(raw), [], JSON.stringify(raw ?? null));
  }
});

test("kind is carried through unnarrowed, because nothing here dispatches on it", () => {
  const parsed = parseTrainingConstructs(
    wire([
      {
        kind: "ritual",
        name: "a",
        signatureRange: { start: { line: 0, character: 0 }, end: { line: 0, character: 1 } },
        state: "trained",
      },
    ]),
  );
  assert.equal(parsed[0]?.kind, "ritual");
  assert.equal(parsed[0]?.state, "trained");
});

// -----------------------------------------------------------------------------
// saving changes nothing
// -----------------------------------------------------------------------------

test("state is a function of the reply alone -- nothing here can observe a save", () => {
  // The acceptance criterion "saving changes no state". Asserted as the two
  // halves that are actually checkable:
  //
  //   here      -- every verdict is derived from the constructs and nothing
  //                else, so repeating the call repeats the answer;
  //   elsewhere -- surfaceGuards.test.ts asserts no training module subscribes
  //                to onDidSaveTextDocument, which is the only channel through
  //                which a save could reach any of this.
  //
  // (An earlier version counted `Function.length` to claim "no third input".
  // That is not what arity means -- it stops at the first defaulted parameter --
  // and it would have failed on any signature change for reasons unrelated to
  // the claim.)
  const constructs = [construct("untrained", "a"), construct("drifted", "b")];
  assert.deepEqual(trainingLensPlans(constructs), trainingLensPlans(constructs));
  assert.deepEqual(countStates(constructs), countStates(constructs));

  // And the same input through a round trip of the wire, which is the path a
  // save would have to change something on.
  const raw = wire(
    constructs.map((c) => ({
      kind: c.kind,
      name: c.name,
      signatureRange: c.signatureRange,
      state: c.state,
    })),
  );
  assert.deepEqual(parseTrainingConstructs(raw), constructs);
  assert.deepEqual(parseTrainingConstructs(raw), parseTrainingConstructs(raw));
});

// -----------------------------------------------------------------------------
// The list the status bar clicks through to (#3763)
// -----------------------------------------------------------------------------

test("the list carries exactly the constructs the status bar counts", () => {
  // THE PROPERTY THAT MATTERS, asserted as an identity rather than as two
  // separate expectations. The number is a claim about a set; a list clicking
  // through to a different set makes the number a lie the moment somebody counts
  // the rows, and that is precisely the sort of disagreement this surface exists
  // to design out.
  const constructs = [
    construct("trained", "a"),
    construct("untrained", "b"),
    construct("seeded", "c"),
    construct("drifted", "d"),
    construct("unknown", "e"),
  ];
  const counts = countStates(constructs);
  const entries = trainingListEntries(constructs);

  assert.equal(entries.length, counts.untrained + counts.drifted);
  assert.equal(statusBarText(counts), "1 untrained · 1 drifted");
  assert.deepEqual(
    entries.map((e) => e.construct.name),
    ["b", "d"],
  );
});

test("the list is in document order, not grouped by state", () => {
  // It is a way back to a place in a file. A developer scanning for the one they
  // were just looking at knows roughly where it sat, not which bucket it fell
  // into.
  const entries = trainingListEntries([
    construct("drifted", "first"),
    construct("trained", "skipped"),
    construct("untrained", "second"),
    construct("drifted", "third"),
  ]);
  assert.deepEqual(
    entries.map((e) => e.construct.name),
    ["first", "second", "third"],
  );
});

test("a row says the kind, the name, the state, and the lens's own sentence", () => {
  const [entry] = trainingListEntries([construct("drifted", "spaceParticipants")]);
  assert.equal(entry?.label, "query spaceParticipants");
  assert.equal(entry?.description, "drifted");
  // The SAME sentence the lens tooltip carries, not a second wording of it: two
  // phrasings for one state are two things to keep in step, and the second one
  // drifts.
  const [plan] = trainingLensPlans([construct("drifted", "spaceParticipants")]);
  assert.equal(entry?.detail, plan?.detail);
});

test("the row keeps the construct, so the reveal uses the server's own range", () => {
  // The click-through must land where the lens and the gutter are anchored. It
  // does that by carrying the construct through rather than by re-deriving a
  // position, which is what constructPanel.ts has to do (it scans the text,
  // having no server answer) and what this deliberately does not.
  const source = construct("untrained", "spaceParticipants");
  const [entry] = trainingListEntries([source]);
  assert.deepEqual(entry?.construct.signatureRange, source.signatureRange);
});

test("a file with nothing untrained or drifted lists nothing", () => {
  // Which is the same set the status bar hides for. The picker's caller turns
  // this into a sentence rather than an empty list -- "everything here is on the
  // cluster" is the good news the surface exists to be able to tell somebody.
  assert.deepEqual(trainingListEntries([construct("trained", "a"), construct("seeded", "b")]), []);
  assert.equal(statusBarText(countStates([construct("trained", "a")])), "");
});

test("an unknown construct is never listed -- a disconnection is not a to-do list", () => {
  // The same rule the gutter holds: `unknown` means no cluster answered, and a
  // row offering to navigate to "something you have not promoted" would be a
  // claim about a cluster nobody asked.
  assert.deepEqual(trainingListEntries([construct("unknown", "a")]), []);
});
