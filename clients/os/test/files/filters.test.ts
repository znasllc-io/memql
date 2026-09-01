import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { applyFilters, DEFAULT_FILTER, type FilesFilter } from "../../src/apps/files/filters";
import { artifactFromRow, type ArtifactRow } from "../../src/apps/files/rows";

// The list derivation (design D1): kind / source / archived / search / folder
// scope / sort, as ONE pure fold over the artifacts snapshot. Client-side on
// purpose -- `folderId == ""` cannot match pre-field rows server-side, and the
// owner-scoped set is small.

function make(over: Partial<Row> & { id: string }): ArtifactRow {
  return artifactFromRow({
    lens: "artifact",
    kind: "file",
    source: "uploaded",
    title: over.id,
    labels: [],
    archived: false,
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  } as Row);
}

const REPORT = make({ id: "a-report", title: "Q3 report.pdf", createdAt: "2026-08-22T10:00:00Z" });
const VIDEO = make({
  id: "a-video",
  title: "demo.mp4",
  folderId: "f-vid",
  createdAt: "2026-08-21T10:00:00Z",
});
const DOC = make({
  id: "a-doc",
  kind: "document",
  source: "uploaded",
  title: "notes.md",
  labels: ["meeting"],
  createdAt: "2026-08-20T10:00:00Z",
});
const MADE = make({
  id: "a-made",
  kind: "generated_output",
  source: "agent_generated",
  title: "summary.txt",
  createdAt: "2026-08-19T10:00:00Z",
});
const OLD_ARCHIVED = make({
  id: "a-archived",
  title: "old.zip",
  archived: true,
  createdAt: "2026-08-01T10:00:00Z",
});
const NOTE = make({ id: "a-note", lens: "record", kind: "note", title: "standup" });

const ALL = [NOTE, DOC, VIDEO, REPORT, MADE, OLD_ARCHIVED];

function ids(filter: Partial<FilesFilter>): string[] {
  return applyFilters(ALL, { ...DEFAULT_FILTER, ...filter }).map((r) => r.id);
}

describe("applyFilters", () => {
  it("never renders a records-lens row, under every filter combination", () => {
    expect(ids({})).not.toContain("a-note");
    expect(ids({ place: "archive", kind: "all", source: "all", search: "standup" })).toEqual([]);
    expect(ids({ folderId: null })).not.toContain("a-note");
  });

  it("scopes to the root by default: filed rows wait behind their folder", () => {
    expect(ids({})).toEqual(["a-report", "a-doc", "a-made"]);
    expect(ids({ folderId: "f-vid" })).toEqual(["a-video"]);
    expect(ids({ folderId: null })).toContain("a-video");
  });

  it("keeps archived rows out of the Library and gives them the Archive place", () => {
    expect(ids({})).not.toContain("a-archived");
    // The Archive root is the whole archived population, not a tree root.
    expect(ids({ place: "archive" })).toEqual(["a-archived"]);
    expect(ids({ place: "archive", folderId: "" })).toContain("a-archived");
  });

  it("the Desktop place mirrors the desk: loose icons at the root, a desk folder's contents one click in", () => {
    const desk = {
      fileArtifactIds: new Set(["a-report"]),
      folderIds: new Set(["f-vid"]),
    };
    const on = (filter: Partial<FilesFilter>) =>
      applyFilters(ALL, { ...DEFAULT_FILTER, ...filter }, desk).map((r) => r.id);
    expect(on({ place: "desktop" })).toEqual(["a-report"]);
    expect(on({ place: "desktop", folderId: "f-vid" })).toEqual(["a-video"]);
    // A search widens to the whole desktop population -- loose icons and
    // desk-folder contents alike, never the rest of the Library.
    expect(on({ place: "desktop", search: "demo" })).toEqual(["a-video"]);
    expect(on({ place: "desktop", search: "meeting" })).toEqual([]);
  });

  it("narrows by kind and by source", () => {
    expect(ids({ kind: "document" })).toEqual(["a-doc"]);
    expect(ids({ source: "agent_generated" })).toEqual(["a-made"]);
  });

  it("searches title, summary and labels across every folder", () => {
    expect(ids({ search: "demo" })).toEqual(["a-video"]);
    expect(ids({ search: "MEETING" })).toEqual(["a-doc"]);
    expect(ids({ search: "nothing-matches" })).toEqual([]);
  });

  it("sorts newest first by default with the ascending toggle honoured", () => {
    expect(ids({ folderId: null })).toEqual(["a-report", "a-video", "a-doc", "a-made"]);
    expect(ids({ folderId: null, sortAscending: true })).toEqual([
      "a-made",
      "a-doc",
      "a-video",
      "a-report",
    ]);
  });

  it("breaks a created-at tie by id so the order never depends on arrival order", () => {
    const twinA = make({ id: "a-twin-a", createdAt: "2026-08-23T10:00:00Z" });
    const twinB = make({ id: "a-twin-b", createdAt: "2026-08-23T10:00:00Z" });
    const forward = applyFilters([twinA, twinB], DEFAULT_FILTER).map((r) => r.id);
    const reversed = applyFilters([twinB, twinA], DEFAULT_FILTER).map((r) => r.id);
    expect(forward).toEqual(reversed);
  });
});
