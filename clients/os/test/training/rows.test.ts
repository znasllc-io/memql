import { describe, expect, it } from "vitest";

import {
  chunkAwaitsReview,
  chunkFromRow,
  fileBelongsHere,
  fileDotTone,
  fileFingerprint,
  fileFromRow,
  groupChunksByDocument,
  rollupDomains,
  runBelongsHere,
  runFromRow,
  runsByFile,
  stageOf,
} from "../../src/apps/training/rows";
import { chunkRow, domainLiteRow, fileRow, runRow } from "./harness";

// The Training app's projections, tested without a DOM. These are the
// predicates the whole app is defined by -- what belongs on the surface, what
// counts as news, and which act a row offers.

describe("fileBelongsHere", () => {
  it("keeps the caller's own files", () => {
    expect(fileBelongsHere(fileFromRow(fileRow({ id: "f-1" })))).toBe(true);
  });

  it("drops an archived file, which lives in the Bin", () => {
    expect(fileBelongsHere(fileFromRow(fileRow({ id: "f-2", archived: true })))).toBe(false);
  });

  it("drops a row with no id", () => {
    expect(fileBelongsHere(fileFromRow({ name: "x" }))).toBe(false);
  });

  // THE RESIDUAL THIS FEED NO LONGER HAS. The plan feed it replaced filtered
  // `requestedBy` client-side, because `v1:planner:plan` declares no tier and
  // a concept that declares nothing admits every subscriber.
  // `v1:library:file` declares the composite owner tier, so admission runs on
  // the subscription too and nobody else's rows arrive -- which is why this
  // predicate takes no viewer id at all. A signature that still accepted one
  // would invite somebody to re-add the filter and conclude it was load-bearing.
  it("takes no viewer id, because the engine scopes the subscription", () => {
    expect(fileBelongsHere.length).toBe(1);
  });
});

describe("fileFingerprint", () => {
  it("changes when the pipeline moves", () => {
    const reading = fileFromRow(fileRow({ id: "f-1", status: "analyzing" }));
    const ready = fileFromRow(fileRow({ id: "f-1", status: "ready" }));
    expect(fileFingerprint(reading)).not.toBe(fileFingerprint(ready));
  });

  it("changes when a domain is taught", () => {
    const before = fileFromRow(fileRow({ id: "f-1" }));
    const after = fileFromRow(fileRow({ id: "f-1", trainedIntoDomainIds: ["d-1"] }));
    expect(fileFingerprint(before)).not.toBe(fileFingerprint(after));
  });

  // A HEARTBEAT IS NOT NEWS. Nothing on a file row moves on a timer, and the
  // summary lands in the same write as `status: "ready"` -- naming it would
  // announce one event twice.
  it("does not change when only the summary is written", () => {
    const a = fileFromRow(fileRow({ id: "f-1", summary: "one" }));
    const b = fileFromRow(fileRow({ id: "f-1", summary: "another" }));
    expect(fileFingerprint(a)).toBe(fileFingerprint(b));
  });
});

describe("runBelongsHere", () => {
  it("keeps an analysis run", () => {
    expect(runBelongsHere(runFromRow(runRow({ id: "r-1" })))).toBe(true);
  });

  it("drops a run of another template", () => {
    const other = runFromRow(runRow({ id: "r-2", automationName: "somethingElse" }));
    expect(runBelongsHere(other)).toBe(false);
  });

  it("drops a run that names no file, which it has nothing to decorate", () => {
    expect(runBelongsHere(runFromRow(runRow({ id: "r-3", input: {} })))).toBe(false);
  });
});

describe("runFromRow", () => {
  it("reads the file id off the run's own input envelope", () => {
    const run = runFromRow(runRow({ id: "r-1", input: { fileId: "file-9" } }));
    expect(run.fileId).toBe("file-9");
  });

  it("reads the outcome the file row cannot say", () => {
    const run = runFromRow(
      runRow({ id: "r-1", outcome: { readable: true, chunks: 12, embedded: 9, summarized: true } }),
    );
    expect(run.passages).toBe(12);
    expect(run.embedded).toBe(9);
    expect(run.summarized).toBe(true);
  });

  // ABSENT IS NOT FALSE. A run in flight has written no outcome, and reading
  // that as "there is nothing in this file" would label every file unreadable
  // for as long as the pass takes -- which is why `stageOf` asks the status
  // first.
  it("reads an absent outcome as zero and false, not as a claim", () => {
    const run = runFromRow(runRow({ id: "r-1", status: "running", outcome: {} }));
    expect(run.readable).toBe(false);
    expect(run.passages).toBe(0);
  });
});

describe("runsByFile", () => {
  it("keeps the NEWEST run per file, which is the first the feed carries", () => {
    const newest = runFromRow(runRow({ id: "r-new", input: { fileId: "f-1" } }));
    const older = runFromRow(runRow({ id: "r-old", input: { fileId: "f-1" } }));
    expect(runsByFile([newest, older]).get("f-1")?.id).toBe("r-new");
  });
});

describe("stageOf", () => {
  const file = (over: Record<string, unknown>) => fileFromRow(fileRow({ id: "f-1", ...over }));
  const run = (over: Record<string, unknown>) => runFromRow(runRow({ id: "r-1", ...over }));

  it("reads a file the cluster is working on as reading", () => {
    expect(stageOf(file({ status: "analyzing" }), undefined)).toBe("reading");
    expect(stageOf(file({ status: "stored" }), undefined)).toBe("reading");
  });

  it("reads a failed file as failed", () => {
    expect(stageOf(file({ status: "failed" }), undefined)).toBe("failed");
  });

  it("reads a ready file with no domains as ready to teach", () => {
    expect(stageOf(file({}), run({}))).toBe("untrained");
  });

  it("reads a ready file with a domain as trained", () => {
    expect(stageOf(file({ trainedIntoDomainIds: ["d-1"] }), run({}))).toBe("trained");
  });

  // The one branch the file row genuinely cannot decide: a photograph and a
  // spreadsheet both end at `ready` with `embeddingStatus: "complete"`.
  it("uses the run to tell a stored photograph from a read document", () => {
    const unreadable = run({ outcome: { readable: false, chunks: 0 } });
    expect(stageOf(file({ summary: "" }), unreadable)).toBe("unreadable");
  });

  // THE FILE LEADS. The upload route writes the file row inside the request
  // and the analysis pass writes the run from a detached goroutine, so a
  // surface that waited for the run would show nothing for the first moments
  // of every upload.
  it("does not wait for a run", () => {
    expect(stageOf(file({ status: "analyzing" }), undefined)).toBe("reading");
    expect(stageOf(file({ trainedIntoDomainIds: ["d-1"] }), undefined)).toBe("trained");
  });

  it("reads a run still in flight as reading rather than unreadable", () => {
    const inFlight = run({ status: "running", outcome: {} });
    expect(stageOf(file({ status: "analyzing" }), inFlight)).toBe("reading");
  });
});

describe("fileDotTone", () => {
  it("is reachable while work is happening", () => {
    expect(fileDotTone("reading")).toBe("reachable");
    expect(fileDotTone("uploading")).toBe("reachable");
  });

  it("is unreachable on a failure", () => {
    expect(fileDotTone("failed")).toBe("unreachable");
  });

  // QUIET IS THE SETTLED STATE: painting every trained file green would make
  // a page of finished work look like a page of running work.
  it("has NO dot for a settled file", () => {
    expect(fileDotTone("untrained")).toBe("unknown");
    expect(fileDotTone("trained")).toBe("unknown");
    expect(fileDotTone("unreadable")).toBe("unknown");
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
