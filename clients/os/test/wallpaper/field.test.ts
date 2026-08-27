import { describe, expect, it } from "vitest";

import { DEFAULT_FIELD, dotAt, generateField, mulberry32 } from "../../src/wallpaper/field";

describe("memory field geometry", () => {
  it("is deterministic under a fixed seed", () => {
    const a = generateField(9, 1440, 900);
    const b = generateField(9, 1440, 900);
    expect(a).toEqual(b);
  });

  it("differs under a different seed", () => {
    const a = generateField(9, 1440, 900);
    const b = generateField(10, 1440, 900);
    expect(a).not.toEqual(b);
  });

  it("stays sparse: dots bounded by the lattice, links rarer than dots", () => {
    const field = generateField(9, 1440, 900);
    const cells = Math.ceil(1440 / DEFAULT_FIELD.cell) * Math.ceil(900 / DEFAULT_FIELD.cell);
    expect(field.dots.length).toBeGreaterThan(0);
    expect(field.dots.length).toBeLessThanOrEqual(cells);
    expect(field.links.length).toBeLessThan(field.dots.length / 2);
  });

  it("links only reach nearby dots", () => {
    const field = generateField(9, 1440, 900);
    for (const l of field.links) {
      const a = field.dots[l.from]!;
      const b = field.dots[l.to]!;
      expect(Math.hypot(a.x - b.x, a.y - b.y)).toBeLessThanOrEqual(DEFAULT_FIELD.linkReach);
    }
  });

  it("drift is a bounded orbit around the base position", () => {
    const dot = generateField(9, 400, 400).dots[0]!;
    for (const t of [0, 10, 100, 1000]) {
      const p = dotAt(dot, t);
      expect(Math.hypot(p.x - dot.x, p.y - dot.y)).toBeLessThanOrEqual(dot.amp * Math.SQRT2 + 0.001);
    }
  });

  it("mulberry32 streams are reproducible", () => {
    const a = mulberry32(42);
    const b = mulberry32(42);
    expect([a(), a(), a()]).toEqual([b(), b(), b()]);
  });
});
