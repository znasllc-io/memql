import { describe, expect, it } from "vitest";

import {
  coreIsConfigured,
  drawnState,
  stopsAreKnown,
  stopsFor,
  type PasskeyReading,
  type StopFacts,
} from "../../src/apps/setup/stops";
import { nextOpen } from "../../src/kit/Rail";
import type { Readiness } from "../../src/live/readiness";
import type { Verdict } from "../../src/system/readinessFold";

// The whole of the wizard's reasoning is here, and none of it needs a DOM.
// What the widget DRAWS is a rail; what it DECIDES -- which stops exist, in
// what order, which one is open, and whether there is anything left to do at
// all -- is this function over four facts.

function verdict(module: string, state: Verdict["state"], core = true): Verdict {
  return { module, state, core, disagreement: [], nodes: [], lanes: [], unknown: [], stale: [], aside: [] };
}

function readiness(loaded: boolean, verdicts: Verdict[]): Readiness {
  const by = new Map(verdicts.map((v) => [v.module, v]));
  return { loaded, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

/** A cluster whose three core modules all report, at the states given. */
function core(ai: Verdict["state"], storage: Verdict["state"], email: Verdict["state"]): Readiness {
  return readiness(true, [
    verdict("ai", ai),
    verdict("storage", storage),
    verdict("email", email),
    // A non-core module reports alongside them and must never reach the rail.
    verdict("campaigns", "unconfigured", false),
  ]);
}

function facts(over: Partial<StopFacts> = {}): StopFacts {
  return {
    readiness: core("unconfigured", "unconfigured", "unconfigured"),
    passkeys: "none",
    authEnabled: true,
    ...over,
  };
}

describe("stopsFor: which stops exist", () => {
  it("answers nothing at all while the readiness feed has not loaded", () => {
    expect(stopsFor(facts({ readiness: undefined }))).toEqual([]);
    expect(stopsFor(facts({ readiness: readiness(false, []) }))).toEqual([]);
  });

  it("answers nothing when the feed is loaded and names no core module", () => {
    // A LOADED FEED WITH NOTHING CORE IN IT IS NOT A CONFIGURED CLUSTER. The
    // rows arrive after the subscription seeds, and reading that gap as "the
    // core is done" would retire the wizard on the boot that needed it.
    expect(stopsFor(facts({ readiness: readiness(true, []) }))).toEqual([]);
    expect(
      stopsFor(facts({ readiness: readiness(true, [verdict("campaigns", "unconfigured", false)]) })),
    ).toEqual([]);
  });

  it("puts the passkey first and then the core modules in manifest order", () => {
    expect(stopsFor(facts()).map((s) => s.id)).toEqual(["passkey", "ai", "storage", "email"]);
  });

  it("leaves every non-core module off the rail", () => {
    expect(stopsFor(facts()).map((s) => s.id)).not.toContain("campaigns");
  });

  it("names each module as the shell names it, and says what it is for", () => {
    const ai = stopsFor(facts()).find((s) => s.id === "ai");
    expect(ai?.name).toBe("Inference");
    expect(ai?.sentence).toBe(
      "Inference needs a provider: a machine on your fleet serving a model, or a federated cloud vendor.",
    );
  });
});

describe("stopsFor: what each stop says", () => {
  it("reads a configured module as done", () => {
    const s = stopsFor(facts({ readiness: core("configured", "unconfigured", "unconfigured") }));
    expect(s.find((x) => x.id === "ai")).toMatchObject({ state: "done", answer: "Set up" });
  });

  it("keeps a partial module waiting, and says so", () => {
    const s = stopsFor(facts({ readiness: core("partial", "unconfigured", "unconfigured") }));
    expect(s.find((x) => x.id === "ai")).toMatchObject({ state: "waiting", answer: "Partly set up" });
  });

  it("says a module nothing reported is not reported, and does not call it done", () => {
    const s = stopsFor(facts({ readiness: core("unreported", "configured", "configured") }));
    expect(s.find((x) => x.id === "ai")).toMatchObject({ state: "waiting", answer: "Not reported" });
    expect(coreIsConfigured(s)).toBe(false);
  });

  // THE RAIL SPEAKS THE KIT'S WORDS (memql#5259). Nodes that differ are "Set
  // up on some nodes", and nodes that answered and could not finish the check
  // are "Could not check" -- neither is a half-filled form, and the second is
  // never "Not reported", because somebody did answer.
  it("names nodes that differ, and a check that failed, in the kit's words", () => {
    const differ = verdict("ai", "partial");
    differ.nodes = [
      { nodeId: "agent-a", nodeType: "agent", state: "configured", reportedAt: "" },
      { nodeId: "bff-a", nodeType: "bff", state: "unconfigured", reportedAt: "" },
    ];
    const failed = verdict("storage", "unreported");
    failed.unknown = ["bff-a"];
    const s = stopsFor(facts({ readiness: readiness(true, [differ, failed, verdict("email", "configured")]) }));
    expect(s.find((x) => x.id === "ai")).toMatchObject({ state: "waiting", answer: "Set up on some nodes" });
    expect(s.find((x) => x.id === "storage")).toMatchObject({ state: "waiting", answer: "Could not check" });
    expect(coreIsConfigured(s)).toBe(false);
  });

  // A NODE BEHIND THE CLUSTER DOES NOT KEEP A STOP OPEN. The fold set it
  // aside, the verdict is configured, and the rail says so -- which is the
  // whole of memql#5259 from the person's side of the screen.
  it("reads a configured module as done however many nodes are catching up", () => {
    const behind = verdict("ai", "configured");
    behind.stale = ["edge-a", "bff-product-a"];
    const s = stopsFor(facts({ readiness: readiness(true, [behind, verdict("storage", "configured"), verdict("email", "configured")]) }));
    expect(s.find((x) => x.id === "ai")).toMatchObject({ state: "done", answer: "Set up" });
  });
});

describe("the passkey stop", () => {
  it("is unknown while the read has not landed, which is not the same as none", () => {
    const s = stopsFor(facts({ passkeys: "unknown" }));
    expect(s[0]).toMatchObject({ id: "passkey", state: "unknown" });
    expect(stopsAreKnown(s)).toBe(false);
  });

  it("is done once one is held", () => {
    const s = stopsFor(facts({ passkeys: "held" }));
    expect(s[0]).toMatchObject({ id: "passkey", state: "done", answer: "Set up" });
  });

  it("carries the one ordering law as its sentence", () => {
    expect(stopsFor(facts())[0]?.sentence).toBe(
      "A sign-in link is the only way back into an account without one, so this comes first.",
    );
  });

  it("is skipped, not waiting, on a cluster that runs with authentication off", () => {
    const s = stopsFor(facts({ authEnabled: false, passkeys: "unknown" }));
    expect(s[0]).toMatchObject({ id: "passkey", state: "skipped", answer: "Not applicable" });
    // ...and an unread passkey no longer silences the rail, because nothing
    // is waiting on it.
    expect(stopsAreKnown(s)).toBe(true);
  });
});

describe("nextOpen: the next unanswered stop, and no Next button", () => {
  it("opens the passkey while it is the first thing outstanding", () => {
    expect(nextOpen(stopsFor(facts({ passkeys: "none" })))).toBe("passkey");
  });

  it("moves to the first module once the passkey is held", () => {
    expect(nextOpen(stopsFor(facts({ passkeys: "held" })))).toBe("ai");
  });

  it("skips a settled module and opens the one after it", () => {
    const s = stopsFor(facts({ passkeys: "held", readiness: core("configured", "unconfigured", "unconfigured") }));
    expect(nextOpen(s)).toBe("storage");
  });

  it("starts at inference when authentication is off", () => {
    expect(nextOpen(stopsFor(facts({ authEnabled: false, passkeys: "unknown" })))).toBe("ai");
  });

  it("opens nothing when everything is settled", () => {
    const s = stopsFor(facts({ passkeys: "held", readiness: core("configured", "configured", "configured") }));
    expect(nextOpen(s)).toBe("");
  });
});

describe("coreIsConfigured: the retire condition", () => {
  it("is false while any stop is outstanding", () => {
    expect(coreIsConfigured(stopsFor(facts({ passkeys: "held", readiness: core("configured", "configured", "unconfigured") })))).toBe(false);
    expect(coreIsConfigured(stopsFor(facts({ passkeys: "none", readiness: core("configured", "configured", "configured") })))).toBe(false);
  });

  it("is false while anything is still unknown -- silence is not completion", () => {
    expect(coreIsConfigured(stopsFor(facts({ passkeys: "unknown", readiness: core("configured", "configured", "configured") })))).toBe(false);
  });

  it("is false when there are no stops at all", () => {
    expect(coreIsConfigured([])).toBe(false);
  });

  it("is true once every stop is done", () => {
    expect(coreIsConfigured(stopsFor(facts({ passkeys: "held", readiness: core("configured", "configured", "configured") })))).toBe(true);
  });

  it("counts a skipped stop as settled", () => {
    const s = stopsFor(facts({ authEnabled: false, passkeys: "unknown", readiness: core("configured", "configured", "configured") }));
    expect(coreIsConfigured(s)).toBe(true);
  });
});

describe("drawnState: the disclosed stop wears the open ring", () => {
  it("promotes the waiting stop the rail has opened, and only that one", () => {
    const s = stopsFor(facts({ passkeys: "held" }));
    const ai = s.find((x) => x.id === "ai")!;
    const storage = s.find((x) => x.id === "storage")!;
    expect(drawnState(ai, "ai")).toBe("open");
    expect(drawnState(storage, "ai")).toBe("waiting");
  });

  it("never promotes a settled stop, even when it is the one open", () => {
    const s = stopsFor(facts({ passkeys: "held" }));
    expect(drawnState(s[0]!, "passkey")).toBe("done");
  });
});

const READINGS: PasskeyReading[] = ["unknown", "none", "held"];

describe("every combination answers something legible", () => {
  it("never produces a stop with no name or an empty state", () => {
    const states: Verdict["state"][] = ["configured", "partial", "unconfigured", "unreported", "notApplicable"];
    for (const p of READINGS) {
      for (const a of states) {
        for (const auth of [true, false]) {
          const s = stopsFor(facts({ passkeys: p, authEnabled: auth, readiness: core(a, a, a) }));
          expect(s.length).toBe(4);
          for (const stop of s) {
            expect(stop.name).not.toBe("");
            expect(stop.sentence).not.toBe("");
            expect(stop.state).not.toBe("");
          }
          // The rail never opens a stop it also calls settled.
          const open = nextOpen(s);
          if (open !== "") {
            expect(s.find((x) => x.id === open)?.state).toBe("waiting");
          }
        }
      }
    }
  });
});
