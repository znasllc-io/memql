import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  addedWithin,
  applyFilters,
  compareRows,
  DEFAULT_FILTER,
  fileKindOf,
  needsWholePlace,
  labelsOf,
  LABEL_ANY,
  type FilesFilter,
} from "../../src/apps/files/filters";
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
    expect(ids({ place: "bin", kind: "all", source: "all", search: "standup" })).toEqual([]);
    expect(ids({ folderId: null })).not.toContain("a-note");
  });

  it("includes every live file in Library and scopes by an explicit folder", () => {
    expect(ids({})).toEqual(["a-report", "a-video", "a-doc", "a-made"]);
    expect(ids({ folderId: "f-vid" })).toEqual(["a-video"]);
    expect(ids({ folderId: null })).toContain("a-video");
  });

  it("keeps archived rows out of the Library and gives them the Bin place", () => {
    expect(ids({})).not.toContain("a-archived");
    // The Bin's root is the whole archived population, not a tree root.
    expect(ids({ place: "bin" })).toEqual(["a-archived"]);
    expect(ids({ place: "bin", folderId: "" })).toContain("a-archived");
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

  it("sorts newest first by default with Oldest first honoured", () => {
    expect(ids({ folderId: null })).toEqual(["a-report", "a-video", "a-doc", "a-made"]);
    expect(ids({ folderId: null, sort: "oldest" })).toEqual([
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

// The label facet (epic memql#5009): a pure fold like every other one here.
describe("the label facet", () => {
  const TAGGED = make({ id: "a-tag-a", title: "one.pdf", labels: ["Client-Acme", "urgent"] });
  const ALSO = make({ id: "a-tag-b", title: "two.pdf", labels: ["urgent"] });
  const BLANKS = make({ id: "a-tag-c", title: "three.pdf", labels: ["  ", "kept"] });

  it("passes everything through when no label is asked for", () => {
    // Same createdAt, so the order is the id tiebreak under the default
    // newest-first direction -- the facet changes membership, never order.
    expect(
      applyFilters([TAGGED, ALSO], { ...DEFAULT_FILTER, label: LABEL_ANY }).map((r) => r.id),
    ).toEqual(["a-tag-b", "a-tag-a"]);
  });

  it("matches EXACTLY -- not a prefix, and never case-folded", () => {
    const on = (label: string) =>
      applyFilters([TAGGED, ALSO], { ...DEFAULT_FILTER, label }).map((r) => r.id);
    expect(on("urgent")).toEqual(["a-tag-b", "a-tag-a"]);
    expect(on("Client-Acme")).toEqual(["a-tag-a"]);
    // Two labels differing only in case are two labels: quietly folding them
    // would show rows under a name that is not written on them. The fuzzy
    // reading is `search`, which already looks inside labels.
    expect(on("client-acme")).toEqual([]);
    expect(on("urg")).toEqual([]);
  });

  it("collects the labels in a population, de-duplicated, alphabetical, blanks dropped", () => {
    expect(labelsOf([TAGGED, ALSO, BLANKS])).toEqual(["Client-Acme", "kept", "urgent"]);
    expect(labelsOf([])).toEqual([]);
  });
});

// The search panel's questions (José, Nov 19 2026): kind of file, Added, sort.
describe("the search panel's kind of file, Added window and sorts", () => {
  const kind = (title: string, mimeType = "", format = "") => fileKindOf({ title, mimeType, format });

  it("reads the name's extension first, then the MIME type, then the recorded format", () => {
    expect(kind("Report.PDF")).toBe("documents");
    expect(kind("deck.pptx")).toBe("documents");
    expect(kind("Site visit 03.jpg")).toBe("images");
    expect(kind("clip.MOV")).toBe("media");
    expect(kind("jingle.m4a")).toBe("media");
    expect(kind("Pricing model.xlsx")).toBe("sheets");
    expect(kind("site.tar.gz")).toBe("archives");
    // The extension wins over a MIME type that says something else.
    expect(kind("table.csv", "text/plain")).toBe("sheets");
    expect(kind("scan", "application/pdf")).toBe("documents");
    expect(kind("export", "text/csv")).toBe("sheets");
    expect(kind("bundle", "application/x-zip-compressed")).toBe("archives");
    expect(kind("voice memo", "audio/mpeg")).toBe("media");
    expect(kind("Q3 figures", "", "spreadsheet")).toBe("sheets");
    expect(kind("export.json", "application/json")).toBe("misc");
    expect(kind("Makefile")).toBe("misc");
    expect(kind(".env")).toBe("misc");
  });

  it("filters a population by kind, and Folders keeps no file at all", () => {
    expect(ids({ type: "documents" })).toEqual(["a-report", "a-doc", "a-made"]);
    expect(ids({ type: "media" })).toEqual(["a-video"]);
    expect(ids({ type: "folders" })).toEqual([]);
  });

  it("keeps what was added inside the window: Today from local midnight, the others rolling", () => {
    const now = new Date(2026, 10, 20, 9, 30).getTime();
    const at = (y: number, m: number, d: number, h = 0) => new Date(y, m, d, h).toISOString();
    expect(addedWithin(at(2026, 10, 20, 0), "today", now)).toBe(true);
    expect(addedWithin(at(2026, 10, 19, 23), "today", now)).toBe(false);
    expect(addedWithin(at(2026, 10, 14, 10), "7d", now)).toBe(true);
    expect(addedWithin(at(2026, 10, 13, 9), "7d", now)).toBe(false);
    expect(addedWithin(at(2026, 9, 22, 10), "30d", now)).toBe(true);
    expect(addedWithin("", "30d", now)).toBe(false);
    expect(addedWithin("", "any", now)).toBe(true);
  });

  it("sorts by name, by size largest first with unknown sizes last, and by kind", () => {
    const rows = [VIDEO, REPORT, DOC, MADE];
    const sizes = new Map([["a-video", 900], ["a-report", 4000]]);
    const order = (sort: Parameters<typeof compareRows>[0]) =>
      [...rows].sort(compareRows(sort, (row) => sizes.get(row.id))).map((row) => row.id);
    expect(order("name")).toEqual(["a-video", "a-doc", "a-report", "a-made"]);
    expect(order("size")).toEqual(["a-report", "a-video", "a-doc", "a-made"]);
    // Documents before Media, as the Kind menu lists them; names break ties.
    expect(order("kind")).toEqual(["a-doc", "a-report", "a-made", "a-video"]);
  });

  it("asks for the whole place only for what the server's pages cannot answer", () => {
    expect(needsWholePlace(DEFAULT_FILTER)).toBe(false);
    expect(needsWholePlace({ ...DEFAULT_FILTER, sort: "oldest" })).toBe(false);
    expect(needsWholePlace({ ...DEFAULT_FILTER, type: "folders" })).toBe(false);
    expect(needsWholePlace({ ...DEFAULT_FILTER, sort: "name" })).toBe(true);
    expect(needsWholePlace({ ...DEFAULT_FILTER, type: "images" })).toBe(true);
    expect(needsWholePlace({ ...DEFAULT_FILTER, added: "7d" })).toBe(true);
  });
});
