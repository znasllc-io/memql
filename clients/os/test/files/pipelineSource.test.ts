import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { SOURCE_VALUES } from "../../src/apps/files/concepts";
import { artifactFromRow, fileStory } from "../../src/apps/files/rows";
import { deriveProvenance } from "../../src/items/provenance";

// A pipeline's files (epic memql#5478, issue memql#5495): a step's archived
// log and its artifacts land in the pipeline owner's Library as source
// "pipeline". The Files list, the inspector and a desk icon all say where one
// came from in the same words, with the dot every cluster source carries: the
// bytes are in this cluster's Library, so the file is reachable.

const PIPELINE_SENTENCE = "Made by a pipeline run";

function pipelineArtifact(over: Partial<Row> = {}): Row {
  return {
    id: "a-tests-log",
    lens: "artifact",
    kind: "file",
    source: "pipeline",
    title: "tests.unit.log",
    summary: "",
    labels: [],
    archived: false,
    createdAt: "2026-10-03T10:00:00Z",
    // Every pipeline file is bound to the work run that made it: the
    // promotion carries the file's producedByRunId onto the index row.
    producedByRunId: "run-7",
    ...over,
  };
}

describe("a pipeline's file", () => {
  it("says it was made by a pipeline run, though a run produced it", () => {
    // NOT "Produced by a plan", which is what any file carrying a run reads
    // as: the source is the more specific fact, and the run id is the
    // inspector's Run fact to show.
    expect(fileStory(artifactFromRow(pipelineArtifact()), null)).toEqual({
      sentence: PIPELINE_SENTENCE,
      tone: "reachable",
      machineNamed: false,
    });
  });

  it("says the same when no run is recorded", () => {
    const story = fileStory(artifactFromRow(pipelineArtifact({ producedByRunId: undefined })), null);
    expect(story.sentence).toBe(PIPELINE_SENTENCE);
    expect(story.tone).toBe("reachable");
  });

  it("gives a desk icon the same origin and the cluster's dot", () => {
    expect(deriveProvenance({ source: "pipeline", producedByWorkerId: "" })).toEqual({
      tone: "reachable",
      origin: PIPELINE_SENTENCE,
    });
  });

  it("is a source the list can be refined by, in the concept's own order", () => {
    expect(SOURCE_VALUES).toContain("pipeline");
    // Read from the concept itself: the filter offers the concept's values
    // and no others, in the order the concept declares them.
    const here = dirname(fileURLToPath(import.meta.url));
    const concepts = readFileSync(join(here, "../../../../dsl/library/concepts.memql"), "utf8");
    const artifact = concepts.slice(concepts.indexOf("concept artifact {"));
    const declared = /\n\s*source\s+enum\(([^)]*)\)/.exec(artifact)?.[1] ?? "";
    const order = [...declared.matchAll(/"([^"]+)"/g)].map((m) => m[1]);
    expect(order).toContain("pipeline");
    const positions = SOURCE_VALUES.map((value) => order.indexOf(value));
    expect(positions.every((at) => at >= 0)).toBe(true);
    expect([...positions].sort((a, b) => a - b)).toEqual(positions);
  });
});
