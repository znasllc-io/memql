import { describe, expect, it } from "vitest";

import { niceMax, niceMin, timeTickIndexes, valueTicks } from "../../../src/kit/statistics/scale";

// A chart's two axes. The value axis has three labelled steps from zero to a
// round top; the time axis labels as many buckets as its width has room for,
// at a regular interval, and always the first and the last.

describe("the top of a value axis", () => {
  it("is the smallest 1, 2, 2.5 or 5 times a power of ten at or above the largest value", () => {
    expect(niceMax(7)).toBe(10);
    expect(niceMax(230)).toBe(250);
    expect(niceMax(1)).toBe(1);
    expect(niceMax(1.5)).toBe(2);
    expect(niceMax(2)).toBe(2);
    expect(niceMax(2.1)).toBe(2.5);
    expect(niceMax(3)).toBe(5);
    expect(niceMax(1000)).toBe(1000);
    expect(niceMax(1204)).toBe(2000);
    expect(niceMax(5001)).toBe(10000);
  });

  it("works below one, for a rate", () => {
    expect(niceMax(0.42)).toBe(0.5);
    expect(niceMax(0.2)).toBe(0.2);
    expect(niceMax(0.21)).toBe(0.25);
    expect(niceMax(0.031)).toBe(0.05);
  });

  it("is 1 when nothing is above zero, so an empty chart still has a scale", () => {
    expect(niceMax(0)).toBe(1);
    expect(niceMax(-3)).toBe(1);
    expect(niceMax(Number.NaN)).toBe(1);
  });

  it("is labelled in three steps from zero", () => {
    expect(valueTicks(230)).toEqual([0, 125, 250]);
    expect(valueTicks(7)).toEqual([0, 5, 10]);
    expect(valueTicks(0.42)).toEqual([0, 0.25, 0.5]);
    expect(valueTicks(0)).toEqual([0, 0.5, 1]);
  });

  it("never labels half of a count, so a small count's axis is zero and its top", () => {
    // Nobody was half a visitor.
    expect(valueTicks(3, true)).toEqual([0, 5]);
    expect(valueTicks(1, true)).toEqual([0, 1]);
    expect(valueTicks(24, true)).toEqual([0, 25]);
    // A whole-number middle stays, and so does a rate's fractional one.
    expect(valueTicks(2, true)).toEqual([0, 1, 2]);
    expect(valueTicks(230, true)).toEqual([0, 125, 250]);
    expect(valueTicks(3)).toEqual([0, 2.5, 5]);
  });
});

describe("the labelled buckets of a time axis", () => {
  it("are every bucket when there is room for them all", () => {
    expect(timeTickIndexes(7, 8)).toEqual([0, 1, 2, 3, 4, 5, 6]);
    expect(timeTickIndexes(8, 8)).toEqual([0, 1, 2, 3, 4, 5, 6, 7]);
  });

  it("are evenly spaced and always the first and the last", () => {
    // Thirty days at room for eight labels: every fifth day, then the last.
    expect(timeTickIndexes(30, 8)).toEqual([0, 5, 10, 15, 20, 25, 29]);
    // The current month and the twelve before it: every other month.
    expect(timeTickIndexes(13, 8)).toEqual([0, 2, 4, 6, 8, 10, 12]);
    // Twenty-four hours: every fourth hour, then the last.
    expect(timeTickIndexes(24, 8)).toEqual([0, 4, 8, 12, 16, 20, 23]);
  });

  it("never label more buckets than there is room for", () => {
    for (const count of [2, 7, 12, 13, 24, 25, 30, 31, 90, 91]) {
      for (const room of [2, 3, 4, 5, 8, 12]) {
        const ticks = timeTickIndexes(count, room);
        expect(ticks.length).toBeLessThanOrEqual(Math.max(room, 2));
        expect(ticks[0]).toBe(0);
        expect(ticks.at(-1)).toBe(count - 1);
      }
    }
  });

  it("drop the label before the last when the two would crowd each other", () => {
    // Every label is owed one slot of the axis (the width over the room).
    // Every second bucket would put a label on 10 and the last on 11: one
    // bucket apart, where a slot is 11 / 8 of a bucket.
    expect(timeTickIndexes(12, 8)).toEqual([0, 2, 4, 6, 8, 11]);
    // Ninety days every thirteenth day leaves 78 eleven days from the last,
    // just short of the 89 / 8 a slot takes.
    expect(timeTickIndexes(90, 8)).toEqual([0, 13, 26, 39, 52, 65, 89]);
  });

  it("are the first and the last when there is room for less than two", () => {
    expect(timeTickIndexes(24, 1)).toEqual([0, 23]);
    expect(timeTickIndexes(24, 0)).toEqual([0, 23]);
  });

  it("are the one bucket of a single point, and none of an empty series", () => {
    expect(timeTickIndexes(1, 8)).toEqual([0]);
    expect(timeTickIndexes(0, 8)).toEqual([]);
  });
});

// The floor of a value axis that can be below zero: a net figure, say, where a
// day of refunds outweighs its sales.
describe("the floor of a value axis", () => {
  it("answers zero for a floor at or above zero and a round number below it", () => {
    expect(niceMin(0)).toBe(0);
    expect(niceMin(12)).toBe(0);
    expect(niceMin(-36)).toBe(-50);
    expect(niceMin(-0.42)).toBe(-0.5);
  });

  it("answers zero for a floor that is not a number", () => {
    expect(niceMin(Number.NaN)).toBe(0);
    expect(niceMin(Number.NEGATIVE_INFINITY)).toBe(0);
  });
});

describe("the value axis's steps below zero", () => {
  it("runs from the floor through zero to the top", () => {
    expect(valueTicks(55, true, -36)).toEqual([-50, 0, 50, 100]);
    expect(valueTicks(55, false, -0.42)).toEqual([-0.5, 0, 50, 100]);
    // The half of the top is left out of a whole-number axis as above zero.
    expect(valueTicks(5, true, -3)).toEqual([-5, 0, 5]);
    // A floor at or above zero is zero: the steps are those it has without one.
    expect(valueTicks(55, true, 12)).toEqual([0, 50, 100]);
    expect(valueTicks(55, true, 0)).toEqual(valueTicks(55, true));
  });
});
