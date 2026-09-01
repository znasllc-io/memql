import { describe, expect, it } from "vitest";

import {
  foldFolderLinkStates,
  linkStateOf,
  rollupLinkState,
  type LinkState,
} from "../../src/apps/files/links";
import { fileRow } from "./harness";

// The origin link states and their folder rollup (epic memql#4783).
//
// Every interesting case here is about ABSENCE, which is why they are worth
// writing down: an absent value renders as nothing at all, so a fold that got
// it wrong would look exactly like a fold that was right.

describe("reading a state off a file row", () => {
  it("reads the three states the engine writes", () => {
    for (const state of ["synced", "stale", "origin_gone"] as const) {
      expect(linkStateOf(fileRow({ id: "f-1", linkState: state }))).toBe(state);
    }
  });

  it("reads ABSENT as no link, never as synced", () => {
    // A browser upload has no origin to link to and every file stored before
    // the field existed has no member. Reading either as `synced` would put an
    // in-sync badge on every file in the Library.
    expect(linkStateOf(fileRow({ id: "f-1" }))).toBe("");
    expect(linkStateOf(fileRow({ id: "f-2", linkState: "" }))).toBe("");
  });

  it("reads a value it does not recognise as no link", () => {
    // A newer engine could write a fourth state. Rendering an unknown one as
    // a badge nobody can interpret is worse than rendering none -- the
    // default branch asserts the least.
    expect(linkStateOf(fileRow({ id: "f-1", linkState: "quarantined" }))).toBe("");
  });
});

describe("rolling a folder up", () => {
  it("reports the WORST, because the point is to make somebody open it", () => {
    expect(rollupLinkState(["synced", "stale"])).toBe("stale");
    expect(rollupLinkState(["stale", "origin_gone"])).toBe("origin_gone");
    expect(rollupLinkState(["origin_gone", "synced", "stale"])).toBe("origin_gone");
  });

  it("answers nothing for a folder holding nothing tracked", () => {
    // Which is most folders. A badge on all of them is noise that makes the
    // few that matter invisible.
    expect(rollupLinkState([])).toBe("");
    expect(rollupLinkState(["", "", ""])).toBe("");
  });

  it("ignores the untracked files beside a tracked one", () => {
    expect(rollupLinkState(["", "stale", ""])).toBe("stale");
  });
});

describe("rolling up through the tree", () => {
  const parents: Record<string, string> = { deep: "mid", mid: "top", top: "" };
  const parentOf = (id: string) => parents[id] ?? "";

  it("flags every ancestor, so the top of a deep tree does not look clean", () => {
    const out = foldFolderLinkStates([{ folderId: "deep", state: "origin_gone" }], parentOf);
    expect(out.get("deep")).toBe("origin_gone");
    expect(out.get("mid")).toBe("origin_gone");
    expect(out.get("top")).toBe("origin_gone");
  });

  it("keeps the worst when two branches disagree", () => {
    const out = foldFolderLinkStates(
      [
        { folderId: "deep", state: "stale" },
        { folderId: "mid", state: "origin_gone" },
      ],
      parentOf,
    );
    expect(out.get("deep")).toBe("stale");
    expect(out.get("mid")).toBe("origin_gone");
    expect(out.get("top")).toBe("origin_gone");
  });

  it("marks no folder at all when nothing is tracked", () => {
    const out = foldFolderLinkStates(
      [
        { folderId: "deep", state: "" },
        { folderId: "mid", state: "" },
      ],
      parentOf,
    );
    expect(out.size).toBe(0);
  });

  it("does not walk out of the root, and does not hang on a corrupt parent chain", () => {
    // A cycle is impossible in the folder model. It is guarded anyway, because
    // the alternative to a slightly wrong badge is a render that never
    // returns.
    const looping = (id: string) => (id === "a" ? "b" : "a");
    const out = foldFolderLinkStates([{ folderId: "a", state: "stale" }], looping);
    expect(out.get("a")).toBe("stale");
    expect(out.get("b")).toBe("stale");
    expect(out.size).toBe(2);
  });

  it("files a ROOT-level file against no folder at all", () => {
    const out = foldFolderLinkStates([{ folderId: "", state: "origin_gone" as LinkState }], parentOf);
    expect(out.size).toBe(0);
  });
});
