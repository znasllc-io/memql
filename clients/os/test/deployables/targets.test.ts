import { describe, expect, it } from "vitest";

import { DEPLOYABLE_KINDS, SITE_CONCEPT, kindLabel } from "../../src/apps/deployables/concepts";
import { SITE_STATUSES } from "../../src/apps/deployables/rows";
import {
  KNOWN_UNOFFERED_KINDS,
  NOT_OFFERED_SENTENCE,
  OFFERED_KINDS,
  STOP_IDS,
  TARGETS,
  UNOFFERED_KIND_LABELS,
  WEB_TARGET,
  targetFor,
} from "../../src/apps/deployables/targets";

// The target registry (design section B) is what every stop renders from, so
// the page has no branch on "which kind is this". One entry, web; the three
// others are the design's table and NOT code, which is the thing worth
// asserting: a registry that grew an ios entry would be a registry the OS
// renders a control for, and nothing can build one.

describe("the target registry", () => {
  it("registers exactly one target, web, over the three offered kinds", () => {
    expect(TARGETS.map((t) => t.id)).toEqual(["web"]);
    expect([...OFFERED_KINDS]).toEqual(["spa", "static", "shopify_storefront"]);
    expect([...WEB_TARGET.offeredKinds]).toEqual([...OFFERED_KINDS]);
  });

  it("offers the same kinds the picker lists, in the same order", () => {
    // The picker entries carry the label and blurb a person reads; the
    // offered list is what the Go parity test reads against the site enum.
    // Two lists that could disagree would let a kind be offered with no
    // picker entry, or listed with no target behind it.
    expect(DEPLOYABLE_KINDS.map((k) => k.value)).toEqual([...OFFERED_KINDS]);
    expect(WEB_TARGET.kinds).toBe(DEPLOYABLE_KINDS);
    for (const kind of OFFERED_KINDS) {
      expect(kindLabel(kind)).not.toBe(kind);
    }
  });

  it("names the site row, its live states and its five stops", () => {
    expect(WEB_TARGET.rowConcept).toBe(SITE_CONCEPT);
    expect([...WEB_TARGET.liveStates]).toEqual([...SITE_STATUSES]);
    expect(WEB_TARGET.buildSurface).toBe("prebuilt");
    expect(WEB_TARGET.stops.map((s) => s.id)).toEqual([...STOP_IDS]);
    expect(WEB_TARGET.stops.map((s) => s.label)).toEqual([
      "Source",
      "What it is",
      "Where it lives",
      "Build",
      "Live",
    ]);
    for (const stop of WEB_TARGET.stops) {
      expect(stop.blurb).not.toBe("");
    }
  });

  it("resolves an offered kind to web, and anything else to nothing", () => {
    for (const kind of OFFERED_KINDS) {
      expect(targetFor(kind)?.id).toBe("web");
    }
    expect(targetFor("banana")).toBeNull();
    // The unoffered kinds are KNOWN and still resolve to nothing: knowing a
    // kind's display name is not the same as having a target for it.
    for (const kind of KNOWN_UNOFFERED_KINDS) {
      expect(targetFor(kind)).toBeNull();
    }
  });

  it("knows the three unoffered kinds by their display names, and offers none of them", () => {
    expect([...KNOWN_UNOFFERED_KINDS]).toEqual(["ios", "android", "macos"]);
    expect(UNOFFERED_KIND_LABELS).toEqual({ ios: "iOS", android: "Android", macos: "macOS" });
    for (const kind of KNOWN_UNOFFERED_KINDS) {
      expect((OFFERED_KINDS as readonly string[]).includes(kind)).toBe(false);
    }
    // The sentence the create form carries today, said once and read from
    // here: it names all three, so a reader learns why the picker stops at
    // three rather than concluding the list is incomplete.
    expect(NOT_OFFERED_SENTENCE).toContain("Android, iOS and macOS");
    expect(NOT_OFFERED_SENTENCE).toContain("not deployables");
  });
});
