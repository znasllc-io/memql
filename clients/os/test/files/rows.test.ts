import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  artifactFingerprint,
  artifactFromRow,
  artifactName,
  fileStory,
  folderFromRow,
  isContentKind,
} from "../../src/apps/files/rows";

// The wire rows the Files app renders, projected into the shapes its surfaces
// read. Pure, fixture-tested, shared by the list, the tree, the inspector and
// the desk popover -- which is what lets the four be checked against each
// other.

function artifact(over: Partial<Row> & { id: string }): Row {
  return {
    lens: "artifact",
    kind: "file",
    source: "uploaded",
    title: "report.pdf",
    summary: "",
    labels: [],
    archived: false,
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  };
}

describe("artifactFromRow", () => {
  it("reads a shape-flattened seed row and a payload-nested event row identically", () => {
    const flat = artifactFromRow(artifact({ id: "a-1", title: "brief.pdf", folderId: "f-1" }));
    const nested = artifactFromRow({
      id: "a-1",
      createdAt: "2026-08-20T10:00:00Z",
      payload: { kind: "file", source: "uploaded", title: "brief.pdf", folderId: "f-1" },
    } as Row);
    expect(nested.id).toBe(flat.id);
    expect(nested.title).toBe(flat.title);
    expect(nested.folderId).toBe("f-1");
    expect(flat.folderId).toBe("f-1");
  });

  it("reads absent folderId as root and absent archived as not archived", () => {
    const row = artifactFromRow(artifact({ id: "a-old" }));
    expect(row.folderId).toBe("");
    expect(row.archived).toBe(false);
  });

  it("keeps labels an array and drops non-string members", () => {
    const row = artifactFromRow(artifact({ id: "a-1", labels: ["tax", 7, "q3"] as never }));
    expect(row.labels).toEqual(["tax", "q3"]);
  });
});

describe("artifactName", () => {
  it("prefers the title and never answers blank", () => {
    expect(artifactName(artifactFromRow(artifact({ id: "a-1", title: "brief.pdf" })))).toBe("brief.pdf");
    expect(artifactName(artifactFromRow(artifact({ id: "a-2", title: "  " })))).toBe("a-2");
  });
});

describe("artifactFingerprint -- what counts as a change", () => {
  const base = artifact({ id: "a-1", updatedAt: "2026-08-20T10:00:00Z" });

  it("is silent on timestamp-only churn (an analysis re-stamp must not strobe)", () => {
    const before = artifactFingerprint(artifactFromRow(base));
    const after = artifactFingerprint(
      artifactFromRow(artifact({ id: "a-1", updatedAt: "2026-08-20T10:05:00Z" })),
    );
    expect(after).toBe(before);
  });

  it.each([
    ["a rename", { title: "renamed.pdf" }],
    ["a move", { folderId: "f-2" }],
    ["an archive", { archived: true }],
    ["a label edit", { labels: ["tax"] }],
    ["an analysis result landing", { summary: "A quarterly report." }],
    ["a validation flip", { validationStatus: "validated" }],
  ])("announces %s", (_what, patch) => {
    const before = artifactFingerprint(artifactFromRow(base));
    const after = artifactFingerprint(artifactFromRow(artifact({ id: "a-1", ...patch })));
    expect(after).not.toBe(before);
  });
});

describe("isContentKind -- the records lens stays out (design D2)", () => {
  it.each(["file", "document", "generated_output"])("admits %s", (kind) => {
    expect(isContentKind(kind)).toBe(true);
  });
  it.each(["note", "todo", "calendar_event", "memory", "live_source", ""])(
    "refuses %s",
    (kind) => {
      expect(isContentKind(kind)).toBe(false);
    },
  );
});

describe("fileStory -- the provenance sentence and its dot", () => {
  it("tells 'uploaded here' with no dot claim beyond the cluster for a browser upload", () => {
    const story = fileStory(artifactFromRow(artifact({ id: "a-1" })), null);
    expect(story.sentence).toBe("Uploaded here");
    expect(story.tone).toBe("reachable");
  });

  it("tells 'uploaded from <machine>' with the machine's presence when a machine is named", () => {
    const row = artifactFromRow(
      artifact({ id: "a-1", producedByWorkerId: "w-1", producedByWorkerName: "rig-7" }),
    );
    expect(fileStory(row, { name: "rig-7", online: true })).toEqual({
      sentence: "Uploaded from rig-7",
      tone: "reachable",
      machineNamed: true,
    });
    expect(fileStory(row, { name: "rig-7", online: false }).tone).toBe("unreachable");
    // No fleet facts yet: the dot never guesses.
    expect(fileStory(row, null).tone).toBe("unknown");
  });

  it("tells 'made on <machine> by computer use' for computer-use output", () => {
    const row = artifactFromRow(
      artifact({
        id: "a-1",
        kind: "generated_output",
        source: "computer_use",
        producedByWorkerId: "w-1",
        producedByWorkerName: "rig-7",
      }),
    );
    const story = fileStory(row, { name: "rig-7", online: true });
    expect(story.sentence).toBe("Made on rig-7 by computer use");
    expect(story.tone).toBe("reachable");
  });

  it("tells 'produced by plan' for a goal's deliverable", () => {
    const row = artifactFromRow(
      artifact({
        id: "a-1",
        kind: "generated_output",
        source: "agent_generated",
        producedByPlanId: "pl-9",
      }),
    );
    const story = fileStory(row, null);
    expect(story.sentence).toBe("Produced by plan pl-9");
    expect(story.tone).toBe("reachable");
  });

  it("falls back to the source's own label for the remaining cluster sources", () => {
    const row = artifactFromRow(artifact({ id: "a-1", source: "workbench_generated" }));
    expect(fileStory(row, null).sentence).toBe("Made on the workbench");
  });
});

describe("folderFromRow", () => {
  it("projects id, name, parent and archived with honest defaults", () => {
    const row = folderFromRow({
      id: "f-1",
      createdAt: "2026-08-20T10:00:00Z",
      payload: { name: "Client videos", parentFolderId: "f-0" },
    } as Row);
    expect(row).toEqual({
      id: "f-1",
      name: "Client videos",
      parentFolderId: "f-0",
      archived: false,
    });
  });
});
