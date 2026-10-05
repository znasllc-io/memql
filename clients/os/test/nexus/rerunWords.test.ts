import { describe, expect, it } from "vitest";

import { rerunInFlightWords } from "../../src/apps/nexus/words";

// What a re-run in flight says, in the words of the act that started it. A
// re-plan (memql#5664) is the failure path's own act: the run continues on the
// new plan from its first step not yet reached, so nothing is "run again".
describe("a re-run in flight, in the words of its act", () => {
  it("says a re-plan continues on the new plan rather than running a step again", () => {
    expect(rerunInFlightWords("replan", "finish", null)).toBe(
      "re-planned after a failure; continuing with the new plan from finish",
    );
  });

  it("keeps the other acts' words", () => {
    expect(rerunInFlightWords("rerun", "draft", 3)).toBe("running draft again as version 3");
    expect(rerunInFlightWords("rerun", "draft", null)).toBe("running draft again");
    expect(rerunInFlightWords("headMove", "draft", null)).toBe(
      "went back in draft; running the steps that depend on it again",
    );
    expect(rerunInFlightWords("branch", "draft", null)).toBe("running from draft on; the steps before it are reused");
  });
});
