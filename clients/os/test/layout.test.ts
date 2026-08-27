import { describe, expect, it } from "vitest";

import { layoutFromMedia, type MatchMedia } from "../src/app/layout";

function stubMedia(flags: { hover?: boolean; fine?: boolean; coarse?: boolean }): MatchMedia {
  return (query: string) => {
    if (query === "(hover: hover)") return { matches: Boolean(flags.hover) };
    if (query === "(pointer: fine)") return { matches: Boolean(flags.fine) };
    if (query === "(pointer: coarse)") return { matches: Boolean(flags.coarse) };
    return { matches: false };
  };
}

describe("layoutFromMedia", () => {
  it("paints desktop chrome only for fine pointer + hover", () => {
    expect(layoutFromMedia(stubMedia({ hover: true, fine: true }))).toBe("desktop");
  });

  it("treats a large coarse viewport as phone, never desktop", () => {
    expect(layoutFromMedia(stubMedia({ hover: false, coarse: true }))).toBe("phone");
  });

  it("does not paint desktop hover chrome on iPad-like coarse + hover", () => {
    expect(layoutFromMedia(stubMedia({ hover: true, coarse: true }))).toBe("ipad");
  });

  it("falls back to phone when matchMedia reports nothing", () => {
    expect(layoutFromMedia(() => ({ matches: false }))).toBe("phone");
  });
});
