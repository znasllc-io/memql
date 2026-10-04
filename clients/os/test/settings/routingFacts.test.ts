import { describe, expect, it } from "vitest";

import {
  DOOR_ORDER,
  EMBEDDINGS_NEVER_DEGRADES,
  LEVELS,
  UNREAD_INFERENCE,
  appList,
  doorIsMetered,
  doorReadings,
  firstOpenDoor,
  inferenceFrom,
  levelReadings,
  type DoorReading,
  type InferenceReading,
} from "../../src/apps/settings/routingFacts";

// The door chain, as arithmetic (epic memql#5153, D1).
//
// ===========================================================================
// WHY THIS SUITE IS PURE AND WHY IT IS THE FIRST ONE
// ===========================================================================
// "Free doors are tried before metered ones" is the product's core promise,
// and it is a claim about an ORDER and four state machines -- not about
// pixels. Asserted through render() it would run behind three layers that can
// each fail for unrelated reasons, and the thing being checked would be the
// least likely suspect when it went red.
//
// ===========================================================================
// THE TWO CLASSES OF DEFECT IT EXISTS TO CATCH
// ===========================================================================
// A door reported OPEN that is not. `localModelCount > 0` is the one that
// reads as an open fleet and is not: a fleet holding four models none of
// which clears the engine's floor cannot take a call, and a surface that
// counted rows would send the reader off to debug why their models are not
// being used when the answer is that they are too small.
//
// A LEVEL THAT NAMES A MODEL NOBODY GAVE IT. Which model serves a level is
// the engine's binding; a plausible client-side guess would land in the same
// ink as a measurement, and a level is exactly the thing a person then acts
// on. So every sentence here is checked against a binding that is ABSENT, and
// the negative control is the same reading WITH one.

/** A read that landed. Overrides go on top of the unread zero value. */
function status(over: Partial<InferenceReading> = {}): InferenceReading {
  return { ...UNREAD_INFERENCE, read: true, ...over };
}

/** Both vendors shut -- the state every cluster is installed in. */
const noVendor = () => ({ state: "unset" as const, said: "" });

/** Both vendors federated. */
const bothVendors = () => ({ state: "open" as const, said: "" });

/** Only Anthropic federated, which is the ordinary paid-vendor cluster. */
const anthropicOnly = (vendor: string) =>
  vendor === "anthropic" ? { state: "open" as const, said: "" } : { state: "unset" as const, said: "" };

/** The fleet door, read out of the full set so the order is exercised too. */
function fleet(over: Partial<InferenceReading> = {}): DoorReading {
  return doorReadings(status(over), noVendor)[0]!;
}

/** The app door, likewise. */
function app(over: Partial<InferenceReading> = {}): DoorReading {
  return doorReadings(status(over), noVendor)[1]!;
}

describe("the order the doors are tried in", () => {
  it("puts the two free doors before the two that bill", () => {
    // Spelled once, here and in DOOR_ORDER. If a fifth door ever arrives, the
    // question this asserts is where it goes -- not whether the list changed.
    expect([...DOOR_ORDER]).toEqual(["fleet", "app", "anthropic", "openai"]);
    expect(DOOR_ORDER.map(doorIsMetered)).toEqual([false, false, true, true]);
  });

  it("reads the four in that order whatever their state", () => {
    // The order is the PRODUCT, so it cannot be a by-product of which doors
    // happen to be open -- a reading that sorted open doors first would look
    // correct on every cluster that has one.
    expect(doorReadings(status(), noVendor).map((d) => d.id)).toEqual([...DOOR_ORDER]);
    expect(doorReadings(UNREAD_INFERENCE, bothVendors).map((d) => d.id)).toEqual([...DOOR_ORDER]);
    expect(
      doorReadings(status({ localEligible: true, localModelCount: 2 }), bothVendors).map((d) => d.id),
    ).toEqual([...DOOR_ORDER]);
  });

  it("marks every door with what a call through it costs", () => {
    const doors = doorReadings(status(), noVendor);
    expect(doors.map((d) => d.metered)).toEqual([false, false, true, true]);
    expect(doors.map((d) => d.kind)).toEqual(["local", "app", "federation", "federation"]);
    // Every door says something. A blank beside a door is a reader's cue to
    // conclude whatever they already believed.
    for (const door of doors) expect(door.said).not.toBe("");
  });
});

describe("the fleet door", () => {
  it("reads a BFF catalog without requiring local dispatch", () => {
    const reading = inferenceFrom({
      fleetCatalogInstalled: true, fleetInferenceInstalled: false,
      localEligible: false, localModelCount: 1, minimumContextWindow: 32768,
    }, "");
    const door = doorReadings(reading, noVendor)[0]!;
    expect(door.state).toBe("half");
    expect(door.said).toMatch(/32,768-token floor/);
  });

  it.each([false, undefined])("calls a missing catalog (%s) unknown, even when local dispatch exists", (catalog) => {
    const reading = inferenceFrom({
      fleetCatalogInstalled: catalog, fleetInferenceInstalled: true,
      localEligible: false, localModelCount: 0,
    }, "");
    const door = doorReadings(reading, noVendor)[0]!;
    expect(door.state).toBe("unknown");
    expect(door.said).toMatch(/fleet inventory cannot be read/);
    expect(door.said).not.toMatch(/deployment setting|pairing one would not/);
  });

  it("keeps the engine eligibility verdict authoritative when catalog metadata is absent", () => {
    const reading = inferenceFrom({
      localEligible: true, localModelCount: 1, eligibleModelIds: ["qwen3.8:27b"],
      fleetInferenceInstalled: false, minimumContextWindow: 32768,
    }, "");
    expect(doorReadings(reading, noVendor)[0]!.state).toBe("open");
  });

  it("is open when the engine says a local model qualifies", () => {
    const door = fleet({
      localEligible: true,
      localModelCount: 3,
      eligibleModelIds: ["qwen3-coder", "llama4"],
      minimumContextWindow: 32000,
      fleetCatalogInstalled: true,
    });
    expect(door.state).toBe("open");
    expect(door.said).toMatch(/2 of 3 models/);
  });

  it("is HALF, not open, when the fleet holds models and none of them qualifies", () => {
    // THE ASSERTION THIS FILE EXISTS FOR. `localEligible` is the engine's own
    // verdict and outranks a count: four models that cannot serve a call are
    // not a door. A reading that keyed on the count would report a working
    // fleet to somebody whose calls are all going to a vendor.
    const door = fleet({
      localEligible: false,
      localModelCount: 4,
      eligibleModelIds: [],
      minimumContextWindow: 32000,
      fleetCatalogInstalled: true,
    });
    expect(door.state).toBe("half");
    expect(door.state).not.toBe("open");
    expect(firstOpenDoor(doorReadings(status({ localEligible: false, localModelCount: 4, fleetCatalogInstalled: true }), noVendor))).toBeNull();
    // And it names the floor rather than paraphrasing it: an operator
    // deciding which model to pull needs the number they are pulling against.
    expect(door.said).toMatch(/32,000-token floor/);
  });

  it("is shut when nothing of yours is offering a model at all", () => {
    const door = fleet({ localEligible: false, localModelCount: 0, fleetCatalogInstalled: true });
    expect(door.state).toBe("shut");
    expect(door.said).toMatch(/No machine you own is offering a model/);
  });

  it("tells unreadable inventory apart from an empty fleet", () => {
    // Same picture, entirely different fixes -- one is on somebody's laptop
    // and the other is on the cluster.
    const noService = fleet({ localEligible: false, localModelCount: 0, fleetCatalogInstalled: false });
    const noModels = fleet({ localEligible: false, localModelCount: 0, fleetCatalogInstalled: true });
    expect(noService.state).toBe("unknown");
    expect(noModels.state).toBe("shut");
    expect(noService.said).not.toBe(noModels.said);
    expect(noService.said).toMatch(/fleet inventory cannot be read/);
  });

  it("is unknown when the status was never read, and carries the reason it has", () => {
    const unread = doorReadings(UNREAD_INFERENCE, noVendor)[0]!;
    expect(unread.state).toBe("unknown");
    expect(unread.state).not.toBe("shut");

    const refused = doorReadings(
      inferenceFrom(null, "inferenceStatus is owner-only"),
      noVendor,
    )[0]!;
    expect(refused.state).toBe("unknown");
    expect(refused.detail).toBe("inferenceStatus is owner-only");
    // "Nobody asked" and "we asked and were refused" are different facts, and
    // neither of them is "the door is shut".
    expect(refused.said).not.toBe(unread.said);
  });
});

describe("the app door", () => {
  it("is open only when an app is eligible AND there is one to run", () => {
    const open = app({ appEligible: true, runnableApps: ["claude-code"], appSessionsInstalled: true });
    expect(open.state).toBe("open");
    expect(open.said).toMatch(/Claude Code/);
    // Eligible with nothing runnable is not an open door: there is nothing on
    // the other side of it.
    expect(app({ appEligible: true, runnableApps: [], appSessionsInstalled: true }).state).toBe("half");
  });

  it("is half when this node can open a session and nobody has signed in", () => {
    expect(app({ appEligible: false, appSessionsInstalled: true }).state).toBe("half");
  });

  it("is shut when this node cannot open an app session at all", () => {
    expect(app({ appEligible: false, appSessionsInstalled: false }).state).toBe("shut");
  });

  it("is unknown before the read lands", () => {
    expect(doorReadings(UNREAD_INFERENCE, noVendor)[1]!.state).toBe("unknown");
  });
});

describe("naming the apps that are signed in", () => {
  it("reads one, two and three in English", () => {
    expect(appList(["claude-code"])).toBe("Claude Code");
    expect(appList(["claude-code", "codex"])).toBe("Claude Code and Codex");
    expect(appList(["claude-code", "codex", "aider"])).toBe("Claude Code, Codex and aider");
  });

  it("passes an app id it does not know THROUGH rather than dropping it", () => {
    // A newer cockpit may report an app this build has never heard of. Naming
    // it as itself is a fact; dropping it silently turns "two apps are signed
    // in" into "one is", which is the reading somebody would act on.
    expect(appList(["gemini-cli"])).toBe("gemini-cli");
    expect(appList(["claude-code", "gemini-cli"])).toBe("Claude Code and gemini-cli");
  });

  it("says so when there is none, rather than rendering nothing", () => {
    expect(appList([])).toBe("No app");
  });
});

describe("which door takes the next call", () => {
  it("resolves a cluster with both a fleet model and a federated vendor to the FLEET", () => {
    // The whole promise, as one assertion: the cheapest door that can answer
    // is the one that does, and a federated vendor does not overtake it.
    const doors = doorReadings(
      status({ localEligible: true, localModelCount: 1, eligibleModelIds: ["qwen3-coder"] }),
      bothVendors,
    );
    const first = firstOpenDoor(doors);
    expect(first?.id).toBe("fleet");
    expect(first?.metered).toBe(false);
  });

  it("falls to the app door before a vendor", () => {
    const doors = doorReadings(
      status({ appEligible: true, runnableApps: ["codex"], appSessionsInstalled: true }),
      bothVendors,
    );
    expect(firstOpenDoor(doors)?.id).toBe("app");
    expect(firstOpenDoor(doors)?.metered).toBe(false);
  });

  it("reaches a vendor only when both free doors are shut", () => {
    const doors = doorReadings(status({ fleetCatalogInstalled: true }), anthropicOnly);
    expect(firstOpenDoor(doors)?.id).toBe("anthropic");
  });

  it("answers null when nothing is open", () => {
    expect(firstOpenDoor(doorReadings(status(), noVendor))).toBeNull();
  });
});

describe("the four levels", () => {
  it("reads all four, in the engine's own order", () => {
    expect(levelReadings(doorReadings(status(), noVendor)).map((l) => l.id)).toEqual([...LEVELS]);
  });

  it("says every level WAITS when no source is ready", () => {
    const levels = levelReadings(doorReadings(status({ fleetCatalogInstalled: true }), noVendor));
    for (const level of levels) {
      expect(level.door).toBeNull();
      expect(level.sentence).toMatch(/No source is ready/);
      // There IS something to do here, so there is advice.
      expect(level.advice).not.toBe("");
    }
  });

  it("says a level is BILLED when only a vendor is open, and names both free ways out", () => {
    const levels = levelReadings(doorReadings(status({ fleetCatalogInstalled: true }), anthropicOnly));
    const strong = levels.find((l) => l.id === "strong")!;
    expect(strong.door?.id).toBe("anthropic");
    expect(strong.door?.metered).toBe(true);
    expect(strong.sentence).toMatch(/goes to Anthropic and is billed/);
    // Both free alternatives, because they are genuinely different fixes and
    // an owner who only hears about one may not have the hardware for it.
    expect(strong.advice).toMatch(/machine you own/);
    expect(strong.advice).toMatch(/Claude Code or Codex/);
    expect(strong.advice).toMatch(/stops being billed/);
  });

  it("attaches NO advice to a level the fleet already takes", () => {
    // Advice on a working state is furniture that never goes away, and a
    // screen of permanent suggestions teaches a reader to stop reading them.
    const levels = levelReadings(
      doorReadings(
        status({ localEligible: true, localModelCount: 2, eligibleModelIds: ["a", "b"] }),
        bothVendors,
      ),
    );
    for (const level of levels) {
      expect(level.door?.kind).toBe("local");
      expect(level.sentence).toMatch(/Your machines take this/);
      expect(level.advice).toBe("");
    }
  });

  it("says the embeddings work WAITS rather than being written wrong", () => {
    // `embeddings` never degrades at any setting: a smaller embedder answers
    // in a different vector space, so a fallback poisons the index instead of
    // slowing it down -- the one level whose failure is silent and permanent.
    expect(EMBEDDINGS_NEVER_DEGRADES).toMatch(/different vector space/);

    const levels = levelReadings(doorReadings(status({ fleetCatalogInstalled: true }), noVendor));
    const embeddings = levels.find((l) => l.id === "embeddings")!;
    const fast = levels.find((l) => l.id === "fast")!;
    expect(embeddings.sentence).toMatch(/waits rather than being written wrong/);
    expect(fast.sentence).toMatch(/waits until one is/);
    // The distinction is the point, so it must not collapse into one sentence.
    expect(embeddings.sentence).not.toBe(fast.sentence);
    expect(embeddings.advice).toMatch(/embedding model/);
  });

  it("NEVER names a model the engine did not give it", () => {
    const doors = doorReadings(
      status({ localEligible: true, localModelCount: 3, eligibleModelIds: ["a", "b"] }),
      noVendor,
    );
    for (const level of levelReadings(doors)) {
      expect(level.model).toBe("");
      expect(level.where).toBe("");
      expect(level.sentence).toBe("Your machines take this. The fleet picks the model.");
      // An unmeasured figure, not a zero: nothing has reported one.
      expect(level.measured.kind).toBe("absent");
    }
  });

  it("names the model once the engine HAS bound one -- the control on the line above", () => {
    // Without this, "never names a model" would pass on a reading that could
    // not name one at all, which is a different and much weaker claim.
    const doors = doorReadings(
      status({ localEligible: true, localModelCount: 3, eligibleModelIds: ["a"] }),
      noVendor,
    );
    const levels = levelReadings(doors, { strong: { model: "qwen3-coder", where: "studio" } });
    const strong = levels.find((l) => l.id === "strong")!;
    expect(strong.model).toBe("qwen3-coder");
    expect(strong.sentence).toContain("qwen3-coder on studio");
    // And a level with no binding still says the fleet picks it, on the same
    // screen: a partial binding must not make the unbound rows look broken.
    expect(levels.find((l) => l.id === "fast")!.sentence).toBe(
      "Your machines take this. The fleet picks the model.",
    );
  });
});

describe("reading `inferenceStatus` off the wire", () => {
  it("keeps an unread reading unread, and carries the error it was given", () => {
    const refused = inferenceFrom(null, "not connected");
    expect(refused.read).toBe(false);
    expect(refused.error).toBe("not connected");
    expect(refused.localEligible).toBe(false);
  });

  it("reads a row through the payload envelope the subscription fold produces", () => {
    const reading = inferenceFrom(
      { payload: { localEligible: true, localModelCount: "3", eligibleModelIds: ["a"] } },
      "",
    );
    expect(reading.read).toBe(true);
    expect(reading.localEligible).toBe(true);
    // The wire carries large integers as strings, so a count that arrives as
    // one is still a count.
    expect(reading.localModelCount).toBe(3);
    expect(reading.eligibleModelIds).toEqual(["a"]);
  });
});
