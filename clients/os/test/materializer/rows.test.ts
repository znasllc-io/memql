import { recipeFacts } from "../../src/apps/materializer/actions";
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
  madeCaption,
  modelsPhrase,
  notSentDetail,
  provenanceClaim,
  sourcesPhrase,
  statusTone,
  statusWord,
} from "../../src/apps/materializer/words";
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

// ---------------------------------------------------------------------------
// What the composer says a finished file is
// ---------------------------------------------------------------------------
//
// "The composed text is in the file" is true of a file a model wrote and false of
// the two the cluster files empty: a blank one (nothing was composed) and a
// described one (empty until the person approves the Library's draft). The
// caption follows what is known: the row (a statement, its sources, whether a
// model was called and whether the file carries its provenance) and, for a file
// this window asked for by describing it, what became of the hand-off.

const BLANK_NOTE = "Created blank. Its history is kept on its composition, not in the file.";
const blankRow = (over: Record<string, unknown> = {}) =>
  compositionRow({ format: "markdown", statement: "", sources: [], modelsUsed: [], provenanceEmbedded: false, provenanceNote: BLANK_NOTE, ...over });
const describedRow = (over: Record<string, unknown> = {}) => blankRow({ statement: "A one page brief", ...over });
const caption = (row: Row, here = false, outcome?: { failure: string }) =>
  madeCaption(compositionFromRow(row), sourcesOf(row), modelsOf(row), here, outcome);

describe("the caption under what was asked for", () => {
  it("says a blank file was created blank, whatever kind of file it is", () => {
    for (const format of ["markdown", "txt", "csv", "json", "html"]) {
      expect(caption(blankRow({ format })), format).toBe("This file was created blank.");
    }
  });

  it("says the description was sent once this window has sent it", () => {
    expect(caption(describedRow(), true, { failure: "" })).toBe("Your description was sent to be written. Open it in the editor to review and approve the draft.");
    // What this window knows wins over what the row shows: it asked for this file by describing it.
    expect(caption(describedRow({ provenanceEmbedded: undefined }), true, { failure: "" })).toBe("Your description was sent to be written. Open it in the editor to review and approve the draft.");
    expect(caption(describedRow({ provenanceEmbedded: undefined }))).toBe("The composed text is in the file.");
  });

  it("leaves a description this window could not send to the notice that says so", () => {
    expect(caption(describedRow(), true, { failure: "revision budget exhausted" })).toBe("");
  });

  it("says only that the file came from a description when this window does not know what became of it", () => {
    // Opened later from the list: the row alone says it was described (a statement, no model, nothing embedded).
    expect(caption(describedRow())).toBe("This file was created from a description. Open it in the editor to continue.");
    // Library files travel with a description, so they do not make it a composed file.
    expect(caption(describedRow({ sources: [{ kind: "library_file", ref: "f1", label: "notes.md", capturedAt: "" }] }))).toBe("This file was created from a description. Open it in the editor to continue.");
    // This window asked for it and has not heard back yet.
    expect(caption(describedRow(), true)).toBe("This file was created from a description. Open it in the editor to continue.");
  });

  it("keeps the composed-file caption for a file a model wrote", () => {
    expect(caption(compositionRow())).toBe("The composed text is in the file.");
    // A Markdown file written from records by the composer: a model was called and the file carries its provenance.
    expect(caption(compositionRow({ format: "markdown", provenanceEmbedded: true }))).toBe("The composed text is in the file.");
  });

  it("says nothing about a file that does not exist yet, or failed to", () => {
    for (const status of ["draft", "composing", "rendering", "failed", "cancelled"]) {
      expect(caption(compositionRow({ status })), status).toBe("");
      expect(caption(blankRow({ status })), status).toBe("");
      expect(caption(describedRow({ status }), true), status).toBe("");
    }
  });
});

describe("a description that was not sent", () => {
  // The notice's own sentence already says the description was not sent; the SDK's lead-in saying so
  // again is dropped, and what is left is the cluster's reason.
  it("shows the cluster's reason without the lead-in", () => {
    expect(notSentDetail("The description could not be sent for writing: revision budget exhausted")).toBe("revision budget exhausted");
    expect(notSentDetail("Stopped before the description was sent.")).toBe("Stopped before the description was sent.");
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


describe("email reference recipes", () => {
  it("retains the organization, output kind, and explicit resource choices", () => {
    const row = compositionRow({ accountIds: ["client-a"], outputKind: "email_template", format: "json", sources: [
      {kind: "library_file", ref: "resources", label: "brand.zip", content: true, includeImages: true},
      {kind: "library_file", ref: "example", label: "example.png", content: true, includeImages: false},
      {kind: "query", ref: "query products()", label: "Products"},
    ] });
    const recipe = recipeFacts(compositionFromRow(row), sourcesOf(row));
    expect(recipe).toMatchObject({ accountIds: ["client-a"], outputKind: "email_template", format: "json", sourceSelectors: [
      {kind: "library_file", selector: "resources", label: "brand.zip", content: true, includeImages: true},
      {kind: "library_file", selector: "example", label: "example.png", content: true, includeImages: false},
      {kind: "concept_query", selector: "query products()", label: "Products"},
    ] });
  });
});
