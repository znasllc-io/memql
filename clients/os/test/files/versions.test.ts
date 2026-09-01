import { describe, expect, it } from "vitest";

import {
  fileHeadFromRow,
  fileVersionFromRow,
  foldVersions,
  headVersionNumber,
  shortDigest,
  versionStory,
  type FileHead,
  type FileVersion,
} from "../../src/apps/files/versions";

// The version fold, pure (epic memql#4806).
//
// Everything the panel draws is a function of two reads, so everything the
// panel can get WRONG is testable here with no browser and no cluster --
// which is the point of keeping the projection out of the component.

function head(over: Partial<FileHead> = {}): FileHead {
  return {
    id: "f-1",
    name: "q3.pdf",
    mimeType: "application/pdf",
    size: 4096,
    sha256: "a".repeat(64),
    format: "pdf",
    status: "ready",
    summary: "",
    versionNumber: 3,
    versionUploadedAt: "2026-08-20T10:00:00Z",
    uploadedFromWorkerId: "",
    uploadedFromWorkerName: "",
    uploadedFromPath: "",
    ...over,
  };
}

function version(n: number, over: Partial<FileVersion> = {}): FileVersion {
  return {
    id: `f-1-v${n}`,
    fileId: "f-1",
    versionNumber: n,
    name: "q3.pdf",
    mimeType: "application/pdf",
    size: 1000 * n,
    sha256: "",
    format: "pdf",
    summary: "",
    uploadedFromWorkerId: "",
    uploadedFromWorkerName: "",
    uploadedFromPath: "",
    uploadedAt: `2026-0${n}-01T10:00:00Z`,
    supersededAt: "2026-08-20T10:00:00Z",
    ...over,
  };
}

describe("foldVersions", () => {
  it("puts the head at the top and marks exactly one entry current", () => {
    const folded = foldVersions(head(), [version(1), version(2)]);
    expect(folded.entries.map((e) => e.versionNumber)).toEqual([3, 2, 1]);
    expect(folded.entries.filter((e) => e.current).map((e) => e.versionNumber)).toEqual([3]);
    expect(folded.total).toBe(3);
    expect(folded.truncated).toBe(false);
  });

  // A supersede is two writes with no transaction across them, and the order
  // is chosen so a crash between them DUPLICATES a version rather than losing
  // one. The fold is where that choice is paid for: the head is the truth, and
  // rendering both would show one upload twice.
  it("lets the head win over a version row claiming the same number", () => {
    const interrupted = version(3, { id: "f-1-v3", name: "stale.pdf", size: 99 });
    const folded = foldVersions(head(), [interrupted, version(1)]);
    expect(folded.entries.map((e) => e.versionNumber)).toEqual([3, 1]);
    const current = folded.entries[0];
    expect(current?.name).toBe("q3.pdf");
    expect(current?.size).toBe(4096);
  });

  // The head SAYS which version it is, so a short read is a fact the panel can
  // state rather than a prefix it shows as if it were everything.
  it("reports truncation against the head's own number, not against a count", () => {
    const folded = foldVersions(head({ versionNumber: 240 }), [version(239), version(238)]);
    expect(folded.total).toBe(240);
    expect(folded.shown).toBe(3);
    expect(folded.truncated).toBe(true);
  });

  it("shows a single-version file as one current entry", () => {
    const folded = foldVersions(head({ versionNumber: 1 }), []);
    expect(folded.entries).toHaveLength(1);
    expect(folded.entries[0]?.current).toBe(true);
    expect(folded.truncated).toBe(false);
  });

  // No head is not "a file with no history" -- it is an answer we do not have,
  // and inventing an empty history for it would be a claim.
  it("renders nothing at all when the head could not be read", () => {
    expect(foldVersions(null, [version(1)]).entries).toEqual([]);
    expect(foldVersions(null, [version(1)]).total).toBe(0);
  });

  it("drops rows that could not be a version", () => {
    const folded = foldVersions(head(), [version(0), version(-2), version(1)]);
    expect(folded.entries.map((e) => e.versionNumber)).toEqual([3, 1]);
  });
});

describe("headVersionNumber", () => {
  // ABSENT IS 1. Every file uploaded before this field existed carries no
  // member at all, and rowNumber answers 0 for it -- which as a version number
  // would label most of the Library "v0".
  it("reads absent, zero and nonsense as version 1", () => {
    expect(headVersionNumber(0)).toBe(1);
    expect(headVersionNumber(-4)).toBe(1);
    expect(headVersionNumber(Number.NaN)).toBe(1);
    expect(headVersionNumber(1)).toBe(1);
    expect(headVersionNumber(7)).toBe(7);
  });

  it("carries through the row projection", () => {
    expect(fileHeadFromRow({ id: "f-1", name: "a.txt" }).versionNumber).toBe(1);
    expect(fileHeadFromRow({ id: "f-1", versionNumber: 5 }).versionNumber).toBe(5);
  });
});

describe("versionStory", () => {
  it("says a version came from a machine, with that machine's presence", () => {
    const facts = { uploadedFromWorkerId: "wrk-1", uploadedFromWorkerName: "MacBook-Pro" };
    expect(versionStory(facts, { name: "MacBook-Pro", online: true })).toEqual({
      sentence: "Uploaded from MacBook-Pro",
      tone: "reachable",
    });
    expect(versionStory(facts, { name: "MacBook-Pro", online: false }).tone).toBe("unreachable");
  });

  // THE DOT NEVER GUESSES: a machine the fleet has nothing to say about is
  // "unknown", which the kit draws as no dot at all.
  it("renders no dot for a machine the fleet cannot speak for", () => {
    expect(
      versionStory({ uploadedFromWorkerId: "wrk-9", uploadedFromWorkerName: "" }, null),
    ).toEqual({ sentence: "Uploaded from one of your machines", tone: "unknown" });
  });

  // A browser CANNOT name a machine. That is a fact about the upload, not a
  // gap -- so it gets its own sentence rather than a hedge.
  it("says a browser upload arrived here", () => {
    expect(
      versionStory({ uploadedFromWorkerId: "", uploadedFromWorkerName: "" }, null),
    ).toEqual({ sentence: "Uploaded here", tone: "reachable" });
  });

  // PROVENANCE IS PER VERSION AND NEVER INHERITED: a file first pushed from a
  // laptop and later replaced from a browser has one entry naming the laptop
  // and one naming nothing, because that is what happened.
  it("gives two versions of one file two different stories", () => {
    const folded = foldVersions(
      head({ versionNumber: 2, uploadedFromWorkerId: "", uploadedFromWorkerName: "" }),
      [version(1, { uploadedFromWorkerId: "wrk-1", uploadedFromWorkerName: "MacBook-Pro" })],
    );
    const stories = folded.entries.map((e) => versionStory(e, { name: "MacBook-Pro", online: true }).sentence);
    expect(stories).toEqual(["Uploaded here", "Uploaded from MacBook-Pro"]);
  });
});

describe("shortDigest", () => {
  it("shortens a real digest and keeps it comparable at a glance", () => {
    expect(shortDigest("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"))
      .toBe("abcdef...456789");
  });

  // ABSENT MEANS NOT MEASURED, never "no hash exists" -- a chunked upload's
  // head can be superseded before the analysis pass has streamed the blob.
  it("renders an unmeasured hash as a dash, not an error", () => {
    expect(shortDigest("")).toBe("--");
    expect(shortDigest("   ")).toBe("--");
  });

  it("leaves a short value alone rather than eliding it to nothing", () => {
    expect(shortDigest("abc123")).toBe("abc123");
  });
});

describe("fileVersionFromRow", () => {
  it("projects the two moments as separate facts", () => {
    const v = fileVersionFromRow({
      id: "f-1-v1",
      fileId: "f-1",
      versionNumber: 1,
      uploadedAt: "2026-06-01T09:00:00Z",
      supersededAt: "2026-08-20T10:00:00Z",
    });
    // WHEN IT ARRIVED and WHEN IT STOPPED BEING CURRENT are different
    // questions, and the row answers both -- reading the intrinsic createdAt
    // instead would answer only the second and label it as the first.
    expect(v.uploadedAt).toBe("2026-06-01T09:00:00Z");
    expect(v.supersededAt).toBe("2026-08-20T10:00:00Z");
  });
});
