import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  compositionFingerprint,
  compositionFromRow,
  modelsOf,
  optionalBool,
  recipeFingerprint,
  recipeFromRow,
  sourcesOf,
  templateFromRow,
} from "../../src/apps/materializer/rows";
import {
  modelsPhrase,
  provenanceClaim,
  sourcesPhrase,
  statusTone,
  statusWord,
} from "../../src/apps/materializer/words";
import { actsFor, stateLine } from "../../src/apps/materializer/acts";
import {
  sanitizeSettings,
  DEFAULT_MATERIALIZER_SETTINGS,
  MATERIALIZER_SECTIONS,
} from "../../src/apps/materializer/settings";

function compositionRow(over: Record<string, unknown> = {}): Row {
  return {
    id: "v1:compose:composition:c1",
    createdAt: "2026-09-05T12:00:00Z",
    ownerUserId: "u1",
    name: "Q3 report",
    statement: "Draft the Q3 report",
    status: "ready",
    format: "pdf",
    outputFileId: "f1",
    sources: [
      { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1", capturedAt: "2026-09-05T12:00:00Z" },
      { kind: "concept_row", ref: "v1:x:invoice#b", label: "INV-2", capturedAt: "2026-09-05T12:00:00Z" },
      { kind: "library_file", ref: "f9", label: "notes.md", capturedAt: "2026-09-05T12:00:00Z" },
    ],
    modelsUsed: [{ provider: "anthropic", model: "claude-sonnet-5", calls: 1, tokens: 4200 }],
    provenanceEmbedded: true,
    provenanceNote: "Provenance is in this PDF's XMP packet.",
    archived: false,
    ...over,
  } as unknown as Row;
}

describe("reading a composition row", () => {
  it("reads the fields the card projects", () => {
    const c = compositionFromRow(compositionRow());
    expect(c.name).toBe("Q3 report");
    expect(c.status).toBe("ready");
    expect(c.format).toBe("pdf");
    expect(c.outputFileId).toBe("f1");
    expect(c.provenanceEmbedded).toBe(true);
  });

  // THE HEADLINE READING RULE OF THIS APP. `rowBool` answers false for a
  // missing key, which collapses "this format cannot carry provenance"
  // and "nothing has decided yet". A composition still composing has not
  // reached the stamp step, and reading that as false would put "the
  // record is the only copy" on a file that will carry its own.
  it("keeps 'not decided yet' apart from 'no'", () => {
    const composing = compositionFromRow(
      compositionRow({ status: "composing", provenanceEmbedded: undefined }),
    );
    expect(composing.provenanceEmbedded).toBeNull();

    const csv = compositionFromRow(compositionRow({ format: "csv", provenanceEmbedded: false }));
    expect(csv.provenanceEmbedded).toBe(false);

    // And the three states must produce three different sentences.
    expect(provenanceClaim(null, "csv")).toBe("");
    expect(provenanceClaim(false, "csv")).toContain("only copy");
    expect(provenanceClaim(true, "pdf")).toBe("the file carries it");
  });

  it("reads an absent list as empty rather than throwing", () => {
    const c = compositionRow({ sources: undefined, modelsUsed: undefined });
    expect(sourcesOf(c)).toEqual([]);
    expect(modelsOf(c)).toEqual([]);
  });

  it("optionalBool answers null for a key that is not a boolean", () => {
    expect(optionalBool({ a: true } as unknown as Row, "a")).toBe(true);
    expect(optionalBool({ a: false } as unknown as Row, "a")).toBe(false);
    expect(optionalBool({} as unknown as Row, "a")).toBeNull();
    expect(optionalBool({ a: "true" } as unknown as Row, "a")).toBeNull();
  });
});

describe("the arrival cue's fingerprint", () => {
  // A HEARTBEAT IS NOT NEWS. `runId` is re-stamped on every attempt of one
  // composition -- a fact about the machinery -- and `createdAt` moves on
  // every write because MemQL is append-only. Naming either would ring the
  // row somebody is already watching.
  it("stays silent on a re-stamped runId", () => {
    const before = compositionFromRow(compositionRow({ runId: "r1" }));
    const after = compositionFromRow(compositionRow({ runId: "r2" }));
    expect(compositionFingerprint(after)).toBe(compositionFingerprint(before));
  });

  it("stays silent on createdAt moving", () => {
    const before = compositionFromRow(compositionRow());
    const after = compositionFromRow(compositionRow({ createdAt: "2026-09-06T09:00:00Z" }));
    expect(compositionFingerprint(after)).toBe(compositionFingerprint(before));
  });

  // THE REACHABLE POSITIVE: without it the two assertions above would pass
  // against a fingerprint that returned a constant.
  it("rings on a rename, a status change and a provenance answer", () => {
    const base = compositionFingerprint(compositionFromRow(compositionRow()));
    expect(compositionFingerprint(compositionFromRow(compositionRow({ name: "Q4 report" })))).not.toBe(base);
    expect(compositionFingerprint(compositionFromRow(compositionRow({ status: "failed" })))).not.toBe(base);
    expect(
      compositionFingerprint(compositionFromRow(compositionRow({ provenanceEmbedded: false }))),
    ).not.toBe(base);
  });

  // A recipe on a schedule touches lastRunAt and runCount forever, so
  // naming either would strobe the list on that schedule's own cycle.
  it("a recipe does not ring when it runs", () => {
    const row = (over: Record<string, unknown> = {}) =>
      recipeFromRow({
        id: "r1",
        name: "Quarterly",
        format: "pdf",
        lastRunAt: "2026-09-01T00:00:00Z",
        runCount: 3,
        ...over,
      } as unknown as Row);
    const before = recipeFingerprint(row());
    const after = recipeFingerprint(row({ lastRunAt: "2026-12-01T00:00:00Z", runCount: 4 }));
    expect(after).toBe(before);
    // Reachable positive.
    expect(recipeFingerprint(row({ name: "Yearly" }))).not.toBe(before);
  });
});

describe("the words", () => {
  it("counts and pluralises sources by kind", () => {
    expect(sourcesPhrase(sourcesOf(compositionRow()))).toBe("2 rows, 1 file");
    expect(sourcesPhrase([])).toBe("no sources");
  });

  // "no model" IS THE ANSWER, NOT AN EM DASH. Everywhere else in this
  // shell an absent figure is a dash with a reason, because absence there
  // means "not measured". Here it means the composition reached no
  // provider, which is the product's headline claim.
  it("says 'no model' rather than treating an empty list as unmeasured", () => {
    expect(modelsPhrase([])).toBe("no model");
    expect(modelsPhrase(modelsOf(compositionRow()))).toBe("anthropic/claude-sonnet-5");
  });

  it("names an unknown status as unknown rather than folding it into a real one", () => {
    // A row written by a newer engine than this bundle must not read as
    // "Draft" -- that is the value that would offer Materialize for a
    // composition already running.
    expect(statusWord("something-new" as never)).toBe("Unknown");
  });

  // The kit's Chip has no warn or error tone, and this app does not widen
  // it: the state is a WORD and the tone only separates settled from
  // in-flight.
  it("keeps status hue off the chip", () => {
    for (const status of ["draft", "composing", "rendering", "ready", "failed", "cancelled"] as const) {
      expect(["neutral", "accent", "muted"]).toContain(statusTone(status));
    }
  });
});

describe("acts follow the state", () => {
  const noDraft = { sourceCount: 0, hasFormat: true, submitting: false };
  const ready = { sourceCount: 2, hasFormat: true, submitting: false };

  // AN ILLEGAL ACT IS ABSENT, NEVER DISABLED.
  it("offers nothing to compose with no sources, and says why on the bar", () => {
    expect(actsFor(null, noDraft)).toEqual([]);
    expect(stateLine(null, noDraft)).toContain("at least one source");
  });

  it("offers Materialize once there is something to compose from", () => {
    expect(actsFor(null, ready).map((a) => a.id)).toEqual(["materialize"]);
  });

  // A RUNNING COMPOSITION OFFERS NO MATERIALIZE: re-materializing would
  // open a second composition against the same sources and produce a
  // second file, with nothing on the page saying which is the deliverable.
  it("offers only Stop while it is running", () => {
    for (const status of ["composing", "rendering"] as const) {
      const acts = actsFor(compositionFromRow(compositionRow({ status })), ready);
      expect(acts.map((a) => a.id)).toEqual(["stop"]);
    }
  });

  // PRIMARY LAST, because that is where the eye lands (rule 12) -- and the
  // primary act on a finished materialization is opening the thing it
  // made. A rendered pass put Archive there instead, which is the act
  // somebody reaches for least.
  it("offers archive, a recipe and the file once it is ready, primary last", () => {
    const acts = actsFor(compositionFromRow(compositionRow()), ready);
    expect(acts.map((a) => a.id)).toEqual(["archive", "saveRecipe", "openFile"]);
    expect(acts[acts.length - 1]?.tone).toBe("primary");
    expect(acts.filter((a) => a.tone === "primary")).toHaveLength(1);
  });

  it("does not offer Save as recipe on a composition that came from one", () => {
    const acts = actsFor(compositionFromRow(compositionRow({ recipeId: "r1" })), ready);
    expect(acts.map((a) => a.id)).not.toContain("saveRecipe");
  });

  // NO RETRY, deliberately: the record holds the sources it RESOLVED
  // rather than the form that produced them, so "retry" would silently
  // mean something slightly different from what was asked for.
  it("offers Start over rather than Retry on a failure, and names the reason", () => {
    const failed = compositionFromRow(
      compositionRow({ status: "failed", failureReason: "object storage refused the upload" }),
    );
    const acts = actsFor(failed, ready);
    expect(acts.map((a) => a.id)).toEqual(["startOver", "archive"]);
    expect(acts.map((a) => a.id)).not.toContain("materialize");
    expect(stateLine(failed, ready)).toContain("object storage refused");
  });

  it("an archived record offers its file and a restore, and nothing else", () => {
    const acts = actsFor(compositionFromRow(compositionRow({ archived: true })), ready);
    expect(acts.map((a) => a.id)).toEqual(["openFile", "restore"]);
  });

  it("never offers more than three acts", () => {
    for (const status of ["draft", "composing", "rendering", "ready", "failed", "cancelled"] as const) {
      for (const archived of [false, true]) {
        const acts = actsFor(compositionFromRow(compositionRow({ status, archived })), ready);
        expect(acts.length).toBeLessThanOrEqual(3);
      }
    }
  });
});

describe("settings", () => {
  it("falls back rather than keeping a section this build does not declare", () => {
    // A preference pointing at a section the manifest lacks navigates the
    // window nowhere, and that failure is silent.
    const repaired = sanitizeSettings({ version: 1, defaultSection: "gone" });
    expect(repaired.defaultSection).toBe(DEFAULT_MATERIALIZER_SETTINGS.defaultSection);
    expect(MATERIALIZER_SECTIONS.map((s) => s.id)).toContain(repaired.defaultSection);
  });

  it("keeps a section this build does declare", () => {
    expect(sanitizeSettings({ version: 1, defaultSection: "materialized" }).defaultSection).toBe(
      "materialized",
    );
  });

  it("falls back on a format nothing can write", () => {
    expect(sanitizeSettings({ version: 1, defaultFormat: "audio" }).defaultFormat).toBe(
      DEFAULT_MATERIALIZER_SETTINGS.defaultFormat,
    );
  });

  it("repairs a document that is not an object at all", () => {
    expect(sanitizeSettings(null)).toEqual(DEFAULT_MATERIALIZER_SETTINGS);
    expect(sanitizeSettings("nonsense")).toEqual(DEFAULT_MATERIALIZER_SETTINGS);
  });
});

describe("a template row", () => {
  it("reads its placeholders so a picker can say what it will ask for", () => {
    const t = templateFromRow({
      id: "t1",
      name: "Acme quarterly",
      format: "docx",
      placeholders: [{ name: "quarter", description: "Which quarter", required: true }],
    } as unknown as Row);
    expect(t.placeholders).toHaveLength(1);
    expect(t.placeholders[0]?.name).toBe("quarter");
    expect(t.placeholders[0]?.required).toBe(true);
  });
});
