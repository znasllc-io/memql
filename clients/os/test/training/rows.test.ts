import { describe, expect, it } from "vitest";

import {
  chunkAwaitsReview,
  chunkFromRow,
  groupChunksByDocument,
  planBelongsHere,
  planDotTone,
  planFileName,
  planFingerprint,
  planFromRow,
  rollupDomains,
} from "../../src/apps/training/rows";
import { chunkRow, domainLiteRow, planRow } from "./harness";

// The Training app's projections, tested without a DOM. These are the
// predicates the whole app is defined by -- what belongs on the surface, what
// counts as news, and what the rollup claims.

describe("planBelongsHere", () => {
  it("keeps the caller's own analyzeFile plans", () => {
    const plan = planFromRow(planRow({ id: "p-1" }));
    expect(planBelongsHere(plan, "u-me")).toBe(true);
  });

  it("DROPS a plan somebody else requested", () => {
    // The subscription is where these arrive: `v1:planner:plan` declares no
    // row-authz tier, so a concept that declares nothing admits every
    // subscriber and other people's plan rows reach this browser.
    const plan = planFromRow(planRow({ id: "p-2", requestedBy: "u-someone-else" }));
    expect(planBelongsHere(plan, "u-me")).toBe(false);
  });

  it("DROPS a plan of another kind", () => {
    const plan = planFromRow(planRow({ id: "p-3", kind: "userGoal" }));
    expect(planBelongsHere(plan, "u-me")).toBe(false);
  });

  it("matches NOTHING while the viewer is unknown", () => {
    // Access resolves asynchronously. "Show every plan in the cluster until we
    // know who is looking" is the exact shape of the bug this guards.
    const mine = planFromRow(planRow({ id: "p-4" }));
    const theirs = planFromRow(planRow({ id: "p-5", requestedBy: "u-someone-else" }));
    expect(planBelongsHere(mine, "")).toBe(false);
    expect(planBelongsHere(theirs, "")).toBe(false);
    expect(planBelongsHere(mine, "   ")).toBe(false);
  });

  it("drops a row with no id", () => {
    const plan = planFromRow({ requestedBy: "u-me", kind: "analyzeFile" });
    expect(planBelongsHere(plan, "u-me")).toBe(false);
  });
});

describe("planFingerprint", () => {
  it("changes on a status transition", () => {
    const queued = planFromRow(planRow({ id: "p-1", status: "queued" }));
    const running = planFromRow(planRow({ id: "p-1", status: "running" }));
    expect(planFingerprint(queued)).not.toBe(planFingerprint(running));
  });

  it("is SILENT on a token counter moving", () => {
    // A HEARTBEAT IS NOT NEWS. `tokenSpent` and the metrics rollup tick for
    // the whole life of an analysis; naming one would pulse the row
    // continuously, which is the standing badge the arrival cue exists not to
    // be.
    const before = planFromRow(planRow({ id: "p-1", status: "running", tokenSpent: 10 }));
    const after = planFromRow(planRow({ id: "p-1", status: "running", tokenSpent: 9000 }));
    expect(planFingerprint(before)).toBe(planFingerprint(after));
  });
});

describe("planDotTone", () => {
  it("speaks the shell's dot language and stays QUIET on success", () => {
    const tone = (status: string) => planDotTone(planFromRow(planRow({ id: "p", status })));
    expect(tone("running")).toBe("reachable");
    expect(tone("failed")).toBe("unreachable");
    expect(tone("cancelled")).toBe("unreachable");
    // No dot: a queued plan has not started and a succeeded one is over.
    expect(tone("queued")).toBe("unknown");
    expect(tone("succeeded")).toBe("unknown");
  });
});

describe("planFileName", () => {
  it("reads the file out of the server's own goal wording", () => {
    expect(planFileName(planFromRow(planRow({ id: "p", goal: "Analyze notes.pdf" })))).toBe(
      "notes.pdf",
    );
  });

  it("renders a goal it cannot parse WHOLE rather than blanking it", () => {
    const goal = "Something else entirely";
    expect(planFileName(planFromRow(planRow({ id: "p", goal })))).toBe(goal);
  });

  it("keeps the goal when the prefix is all there is", () => {
    expect(planFileName(planFromRow(planRow({ id: "p", goal: "Analyze " })))).toBe("Analyze");
  });
});

describe("chunkFromRow", () => {
  it("reads an ABSENT validationStatus as the concept's default", () => {
    // A concept field is not a readable field: before the shape carried
    // `validationStatus`, every chunk arrived without the key. Reading that as
    // "" would have made every chunk fall out of the queue.
    const chunk = chunkFromRow({ id: "c-1", text: "x", domainId: "d" });
    expect(chunk.validationStatus).toBe("unvalidated");
  });

  it("reads a blank validationStatus the same way", () => {
    const chunk = chunkFromRow(chunkRow({ id: "c-1", validationStatus: "   " }));
    expect(chunk.validationStatus).toBe("unvalidated");
  });
});

describe("chunkAwaitsReview", () => {
  it("admits an unvalidated chunk", () => {
    expect(chunkAwaitsReview(chunkFromRow(chunkRow({ id: "c-1" })))).toBe(true);
  });

  it("refuses a decided chunk", () => {
    expect(
      chunkAwaitsReview(chunkFromRow(chunkRow({ id: "c-1", validationStatus: "validated" }))),
    ).toBe(false);
    expect(
      chunkAwaitsReview(chunkFromRow(chunkRow({ id: "c-2", validationStatus: "rejected" }))),
    ).toBe(false);
  });

  it("refuses a SUPERSEDED chunk that is still unvalidated", () => {
    // Supersession is the other axis, and it is checked first: retrieval
    // already excludes the chunk, so asking somebody to approve it would be
    // asking for a decision about content the engine has stopped using.
    const chunk = chunkFromRow(
      chunkRow({ id: "c-1", superseded: true, supersededReason: "superseded by 2026 figures" }),
    );
    expect(chunk.validationStatus).toBe("unvalidated");
    expect(chunkAwaitsReview(chunk)).toBe(false);
  });
});

describe("groupChunksByDocument", () => {
  it("groups by documentId and puts the corpus LAST", () => {
    const chunks = [
      chunkRow({ id: "c-1", documentId: "doc-a" }),
      chunkRow({ id: "c-2", documentId: "" }),
      chunkRow({ id: "c-3", documentId: "doc-b" }),
      chunkRow({ id: "c-4", documentId: "doc-a" }),
    ].map(chunkFromRow);

    const groups = groupChunksByDocument(chunks);
    expect(groups.map((g) => g.id)).toEqual(["doc-a", "doc-b", ""]);
    expect(groups[0]?.chunks.map((c) => c.id)).toEqual(["c-1", "c-4"]);
    expect(groups[2]?.label).toBe("Seeded corpus");
  });

  it("keeps the input's order for the document groups", () => {
    // The reads that feed this are already sorted newest-first by the engine.
    // A second sort here would be a second opinion about an ordering the
    // caller can see.
    const chunks = [
      chunkRow({ id: "c-1", documentId: "doc-z" }),
      chunkRow({ id: "c-2", documentId: "doc-a" }),
    ].map(chunkFromRow);
    expect(groupChunksByDocument(chunks).map((g) => g.id)).toEqual(["doc-z", "doc-a"]);
  });

  it("produces no corpus group when every chunk has a document", () => {
    const chunks = [chunkRow({ id: "c-1", documentId: "doc-a" })].map(chunkFromRow);
    expect(groupChunksByDocument(chunks)).toHaveLength(1);
  });
});

describe("rollupDomains", () => {
  it("counts the three states and SUMS to the total", () => {
    const rows = [
      domainLiteRow("domain-sales", "unvalidated"),
      domainLiteRow("domain-sales", "validated"),
      domainLiteRow("domain-sales", "rejected"),
      domainLiteRow("domain-sales", "validated"),
      domainLiteRow("domain-hr", "unvalidated"),
    ];
    const [hr, sales] = rollupDomains(rows);
    expect(sales).toEqual({
      domainId: "domain-sales",
      total: 4,
      validated: 2,
      unvalidated: 1,
      rejected: 1,
    });
    expect(sales!.validated + sales!.unvalidated + sales!.rejected).toBe(sales!.total);
    expect(hr!.domainId).toBe("domain-hr");
  });

  it("counts an ABSENT or unrecognised status as unvalidated, so the parts still sum", () => {
    // A fourth bucket for "something else" would make the rollup stop adding
    // up, and a reader checking the arithmetic is the reader this is for.
    const rows = [
      { domainId: "domain-sales" },
      domainLiteRow("domain-sales", ""),
      domainLiteRow("domain-sales", "something-new"),
    ];
    const [sales] = rollupDomains(rows);
    expect(sales).toEqual({
      domainId: "domain-sales",
      total: 3,
      validated: 0,
      unvalidated: 3,
      rejected: 0,
    });
  });

  it("drops a row with no domain and sorts by name", () => {
    const rows = [
      domainLiteRow("zeta", "validated"),
      domainLiteRow("", "validated"),
      domainLiteRow("alpha", "validated"),
    ];
    expect(rollupDomains(rows).map((r) => r.domainId)).toEqual(["alpha", "zeta"]);
  });
});
