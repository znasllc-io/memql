import { describe, expect, it, vi } from "vitest";

import { uploadDroppedTree } from "../../src/apps/files/uploadTree";
import type { DroppedFile } from "../../src/items/folderDrop";

// The folder-drop orchestration (design D3): create the Library folder tree
// FIRST, then upload every file into its folder with modest concurrency. A
// partial failure leaves landed files landed and lists exactly the failures.

function dropped(name: string, dirPath: string[]): DroppedFile {
  return { file: new File(["x"], name), dirPath };
}

describe("uploadDroppedTree", () => {
  it("creates each folder once, parents before children, then uploads into them", async () => {
    const created: Array<{ name: string; parent: string }> = [];
    const uploaded: Array<{ name: string; folderId: string }> = [];
    let seq = 0;
    const result = await uploadDroppedTree(
      [
        dropped("a.mp4", ["Client videos"]),
        dropped("b.mov", ["Client videos", "raw"]),
        dropped("c.mov", ["Client videos", "raw"]),
        dropped("loose.txt", []),
      ],
      "f-target",
      {
        createFolder: async (name, parentFolderId) => {
          created.push({ name, parent: parentFolderId });
          seq += 1;
          return `f-${seq}`;
        },
        uploadFile: async (file, folderId) => {
          uploaded.push({ name: file.name, folderId });
        },
        concurrency: 2,
        onFileSettled: () => {},
      },
    );
    expect(created).toEqual([
      { name: "Client videos", parent: "f-target" },
      { name: "raw", parent: "f-1" },
    ]);
    expect(uploaded.find((u) => u.name === "a.mp4")?.folderId).toBe("f-1");
    expect(uploaded.find((u) => u.name === "b.mov")?.folderId).toBe("f-2");
    expect(uploaded.find((u) => u.name === "loose.txt")?.folderId).toBe("f-target");
    expect(result.failures).toEqual([]);
    expect(result.landed).toBe(4);
  });

  it("a partial failure leaves landed files landed and names exactly the failures", async () => {
    const settled = vi.fn();
    const result = await uploadDroppedTree(
      [dropped("ok.txt", []), dropped("bad.txt", []), dropped("ok2.txt", [])],
      "",
      {
        createFolder: async () => "f-x",
        uploadFile: async (file) => {
          if (file.name === "bad.txt") throw new Error("the cluster refused: quota");
        },
        concurrency: 1,
        onFileSettled: settled,
      },
    );
    expect(result.landed).toBe(2);
    expect(result.failures.map((f) => [f.file.name, f.error])).toEqual([
      ["bad.txt", "the cluster refused: quota"],
    ]);
    expect(settled).toHaveBeenCalledTimes(3);
  });

  it("a folder named with a space never collides with two nested names", async () => {
    // ["Client videos"] and ["Client", "videos"] are different places; a
    // path key joined on a space would fold them into one folder.
    const created: Array<{ name: string; parent: string }> = [];
    let seq = 0;
    const uploaded: Array<{ name: string; folderId: string }> = [];
    await uploadDroppedTree(
      [dropped("a.txt", ["Client videos"]), dropped("b.txt", ["Client", "videos"])],
      "",
      {
        createFolder: async (name, parent) => {
          created.push({ name, parent });
          seq += 1;
          return `f-${seq}`;
        },
        uploadFile: async (file, folderId) => void uploaded.push({ name: file.name, folderId }),
        concurrency: 1,
        onFileSettled: () => {},
      },
    );
    expect(created.map((c) => c.name).sort()).toEqual(["Client", "Client videos", "videos"]);
    expect(uploaded.find((u) => u.name === "a.txt")?.folderId).not.toBe(
      uploaded.find((u) => u.name === "b.txt")?.folderId,
    );
  });

  it("a folder-creation failure fails the files under it and nothing else", async () => {
    const result = await uploadDroppedTree(
      [dropped("in.txt", ["broken"]), dropped("out.txt", [])],
      "",
      {
        createFolder: async () => {
          throw new Error("folders are refused today");
        },
        uploadFile: async () => {},
        concurrency: 1,
        onFileSettled: () => {},
      },
    );
    expect(result.landed).toBe(1);
    expect(result.failures.map((f) => f.file.name)).toEqual(["in.txt"]);
    expect(result.failures[0]?.error).toBe("folders are refused today");
  });
});
