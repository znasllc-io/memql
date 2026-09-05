import { describe, expect, it } from "vitest";

import { NICKNAME_SPACE, NICKNAME_WORDS as WORDS, generateNickname } from "../../src/apps/deployables/packages/nickname";

// A generated address is one somebody may have to read off a screen or say to
// a colleague, so the shape is the contract.

describe("a generated address", () => {
  it("is two ordinary words joined by a hyphen", () => {
    expect(generateNickname(() => 0)).toMatch(/^[a-z]+-[a-z]+$/);
  });

  it("is a valid DNS label: lowercase letters and one hyphen, never an underscore", () => {
    // Docker joins with an underscore, which is not valid in a hostname label
    // -- and this value becomes one.
    for (let i = 0; i < 400; i++) {
      const name = generateNickname();
      expect(name).toMatch(/^[a-z]+-[a-z]+$/);
      expect(name).not.toContain("_");
      expect(name.length).toBeLessThanOrEqual(63);
    }
  });

  it("stays short enough to be memorable, because every word obeys the rule", () => {
    // ASSERT THE RULE ON THE WORDS, not on samples of the output. Sampling
    // found `wandering` (nine letters) only because one draw in a few hundred
    // happened to pair it with an eight-letter noun -- a real inconsistency
    // between the stated rule and the data, discovered by luck. Four to eight
    // letters each bounds every pair at 17 without needing a draw at all.
    for (const word of WORDS) {
      expect(word.length).toBeGreaterThanOrEqual(4);
      expect(word.length).toBeLessThanOrEqual(8);
    }
    for (let i = 0; i < 400; i++) expect(generateNickname().length).toBeLessThanOrEqual(17);
  });

  it("draws the two halves independently, so the space is the product", () => {
    // The negative control on the obvious bug: one index used for both lists
    // would give `amber-acorn`, `ancient-anchor`, ... and a space the size of
    // the SHORTER list rather than the product.
    expect(NICKNAME_SPACE).toBeGreaterThanOrEqual(9000);
    const seen = new Set<string>();
    for (let i = 0; i < 3000; i++) seen.add(generateNickname());
    expect(seen.size).toBeGreaterThan(1500);
  });

  it("never runs off the end of a list when random() returns 1", () => {
    // Math.random() is [0,1), but an injected one need not be, and an
    // off-the-end pick would render "undefined-undefined" into somebody's URL.
    expect(generateNickname(() => 1)).toMatch(/^[a-z]+-[a-z]+$/);
    expect(generateNickname(() => 0.999999999)).toMatch(/^[a-z]+-[a-z]+$/);
  });

  it("carries no proper nouns, so nobody's name lands on a stranger's address", () => {
    const names = new Set<string>();
    for (let i = 0; i < 4000; i++) for (const half of generateNickname().split("-")) names.add(half);
    for (const w of names) expect(w).toBe(w.toLowerCase());
  });
});
