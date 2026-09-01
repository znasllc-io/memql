import { describe, expect, it } from "vitest";

import {
  MAX_DROP_DEPTH,
  MAX_DROP_FILES,
  walkEntries,
  type EntryLike,
} from "../../src/items/folderDrop";

// The folder-drop walker (design D3): a bounded directory-entry walk that
// yields every file with the folder path it should land under -- or one
// refusal sentence when the drop is past what a browser upload should carry.

function fileEntry(name: string): EntryLike {
  return {
    name,
    isFile: true,
    isDirectory: false,
    file: (cb) => cb(new File(["x"], name)),
  };
}

function dirEntry(name: string, children: EntryLike[]): EntryLike {
  let served = false;
  return {
    name,
    isFile: false,
    isDirectory: true,
    createReader: () => ({
      // The platform contract: readEntries returns batches and then an empty
      // batch. Serving everything once and then [] mirrors Chrome.
      readEntries: (cb) => {
        if (served) cb([]);
        else {
          served = true;
          cb(children);
        }
      },
    }),
  };
}

describe("walkEntries", () => {
  it("yields files with their folder paths, tree order", async () => {
    const out = await walkEntries([
      dirEntry("Client videos", [
        fileEntry("a.mp4"),
        dirEntry("raw", [fileEntry("b.mov")]),
      ]),
      fileEntry("notes.txt"),
    ]);
    expect(out.refusal).toBe("");
    expect(out.files.map((f) => [f.dirPath.join("/"), f.file.name])).toEqual([
      [["Client videos"].join("/"), "a.mp4"],
      ["Client videos/raw", "b.mov"],
      ["", "notes.txt"],
    ]);
  });

  it("refuses a drop past the file bound with a sentence naming it", async () => {
    const many = Array.from({ length: MAX_DROP_FILES + 1 }, (_, i) => fileEntry(`f${i}.txt`));
    const out = await walkEntries([dirEntry("big", many)]);
    expect(out.files).toEqual([]);
    expect(out.refusal).toContain(String(MAX_DROP_FILES));
  });

  it("refuses a drop nested past the depth bound", async () => {
    let leaf: EntryLike = fileEntry("deep.txt");
    for (let i = 0; i <= MAX_DROP_DEPTH; i += 1) leaf = dirEntry(`d${i}`, [leaf]);
    const out = await walkEntries([leaf]);
    expect(out.files).toEqual([]);
    expect(out.refusal).toContain(String(MAX_DROP_DEPTH));
  });
});
