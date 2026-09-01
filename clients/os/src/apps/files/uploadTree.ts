import type { DroppedFile } from "../../items/folderDrop";

// The folder-drop orchestration (design D3): the Library folder tree first,
// then the files, with modest concurrency.
//
// FOLDERS BEFORE FILES, because a file upload names its folder at init and
// a folder is one mutation -- the cheap half goes first so no file ever
// lands unfiled and gets re-filed later. Each path is created ONCE (the walk
// can yield fifty files in one folder); a failed folder fails exactly the
// files under it, with the folder's own refusal as their reason, and
// everything else proceeds -- a partial failure leaves landed files landed.

export interface UploadTreePorts {
  /** Create one folder under a parent ("" = the drop target's own scope). */
  createFolder: (name: string, parentFolderId: string) => Promise<string>;
  uploadFile: (file: File, folderId: string) => Promise<void>;
  /** In-flight uploads at once. Modest on purpose: the chunked path is
   *  already sequential per file, and a browser saturates long before 3. */
  concurrency: number;
  /** Called once per file, success or failure -- the aggregate progress. */
  onFileSettled: (file: File, error: string) => void;
  /** Folder paths already resolved by an earlier run -- what a RETRY hands
   *  back so a re-run creates no duplicate siblings (names are not unique). */
  knownFolders?: ReadonlyMap<string, string>;
}

export interface UploadTreeResult {
  landed: number;
  failures: Array<{ file: File; dirPath: string[]; error: string }>;
  /** Every path this run resolved, for the retry above. */
  folderIdByPath: Map<string, string>;
}

/** Path keys join on the escaped form of a byte no folder name can carry --
 *  a name with a space must not collide with two nested names. Written as an
 *  ESCAPE, never a raw control byte: a literal NUL in source turns the file
 *  binary to grep and to every repo-walking gate. */
export const TREE_PATH_SEP = "\u001f";

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function treePathKey(dirPath: readonly string[]): string {
  return dirPath.join(TREE_PATH_SEP);
}

export async function uploadDroppedTree(
  files: readonly DroppedFile[],
  targetFolderId: string,
  ports: UploadTreePorts,
): Promise<UploadTreeResult> {
  // Resolve every distinct folder path to an id, parents before children.
  // A path whose creation refused poisons its descendants with the same
  // sentence -- creating children under a folder that does not exist would
  // orphan them somewhere the person never named.
  const folderIdByPath = new Map<string, string>([["", targetFolderId]]);
  for (const [key, id] of ports.knownFolders ?? []) folderIdByPath.set(key, id);
  const refusedByPath = new Map<string, string>();
  // EVERY PREFIX of every file's path, not just the paths files sit in
  // directly: a file at a/b/c needs a and a/b to exist first, whether or not
  // any file lives in them. (A directory with no files anywhere beneath it
  // yields no path here and is deliberately not created -- an upload is
  // about the files.)
  const wanted = new Set<string>();
  for (const f of files) {
    for (let depth = 1; depth <= f.dirPath.length; depth += 1) {
      wanted.add(treePathKey(f.dirPath.slice(0, depth)));
    }
  }
  const paths = [...wanted].sort(
    (a, b) => a.split(TREE_PATH_SEP).length - b.split(TREE_PATH_SEP).length || a.localeCompare(b),
  );
  for (const key of paths) {
    if (folderIdByPath.has(key)) continue;
    const segments = key.split(TREE_PATH_SEP);
    const parentKey = segments.slice(0, -1).join(TREE_PATH_SEP);
    const refusedAbove = refusedByPath.get(parentKey);
    if (refusedAbove !== undefined) {
      refusedByPath.set(key, refusedAbove);
      continue;
    }
    const parentId = folderIdByPath.get(parentKey) ?? targetFolderId;
    try {
      const id = await ports.createFolder(segments.at(-1) ?? "", parentId);
      folderIdByPath.set(key, id);
    } catch (err) {
      refusedByPath.set(key, describe(err));
    }
  }

  // The files, a bounded pool at a time.
  const failures: UploadTreeResult["failures"] = [];
  let landed = 0;
  const queue = [...files];
  const workers = Array.from({ length: Math.max(1, ports.concurrency) }, async () => {
    for (;;) {
      const next = queue.shift();
      if (!next) return;
      const key = treePathKey(next.dirPath);
      const refused = refusedByPath.get(key);
      if (refused !== undefined) {
        failures.push({ file: next.file, dirPath: next.dirPath, error: refused });
        ports.onFileSettled(next.file, refused);
        continue;
      }
      const folderId = folderIdByPath.get(key) ?? targetFolderId;
      try {
        await ports.uploadFile(next.file, folderId);
        landed += 1;
        ports.onFileSettled(next.file, "");
      } catch (err) {
        const error = describe(err);
        failures.push({ file: next.file, dirPath: next.dirPath, error });
        ports.onFileSettled(next.file, error);
      }
    }
  });
  await Promise.all(workers);
  return { landed, failures, folderIdByPath };
}
