// The folder-drop walk (design D3): a dropped directory becomes files with
// the folder paths they should land under, bounded before anything uploads.
//
// BOUNDED, AND THE BOUND REFUSES THE WHOLE DROP. <= 500 files, depth <= 12
// (the folder tree's own cap). A drop past either answers one in-surface
// sentence and uploads NOTHING -- half a folder landing silently is a backup
// that lies, which is the failure mode the D12 story exists to prevent. The
// alternatives the sentence names (cockpit) are the right tool at that size.

export const MAX_DROP_FILES = 500;
export const MAX_DROP_DEPTH = 12;

/** The subset of FileSystemEntry the walk reads -- injectable for tests,
 *  since jsdom ships no directory entries. */
export interface EntryLike {
  name: string;
  isFile: boolean;
  isDirectory: boolean;
  file?: (cb: (file: File) => void, err?: (e: unknown) => void) => void;
  createReader?: () => {
    readEntries: (cb: (entries: EntryLike[]) => void, err?: (e: unknown) => void) => void;
  };
}

export interface DroppedFile {
  file: File;
  /** Folder path RELATIVE to the drop target, [] for a loose file. */
  dirPath: string[];
}

export interface DroppedTree {
  files: DroppedFile[];
  /** "" when the drop is within bounds; otherwise the sentence to render. */
  refusal: string;
}

function fileOf(entry: EntryLike): Promise<File> {
  return new Promise((resolve, reject) => {
    if (!entry.file) {
      reject(new Error(`entry ${entry.name} has no file()`));
      return;
    }
    entry.file(resolve, reject);
  });
}

async function allEntries(entry: EntryLike): Promise<EntryLike[]> {
  const reader = entry.createReader?.();
  if (!reader) return [];
  const out: EntryLike[] = [];
  // readEntries answers in batches (Chrome caps one call at 100) and then an
  // empty batch; a single call silently truncates a big folder.
  for (;;) {
    const batch = await new Promise<EntryLike[]>((resolve, reject) =>
      reader.readEntries(resolve, reject),
    );
    if (batch.length === 0) return out;
    out.push(...batch);
  }
}

class DropRefused extends Error {}

export async function walkEntries(entries: readonly EntryLike[]): Promise<DroppedTree> {
  const files: DroppedFile[] = [];

  async function walk(entry: EntryLike, dirPath: string[], depth: number): Promise<void> {
    if (depth > MAX_DROP_DEPTH) {
      throw new DropRefused(
        `This drop nests deeper than ${MAX_DROP_DEPTH} levels, which is past what a folder here can hold. The cockpit on one of your machines is the right tool for a tree that size.`,
      );
    }
    if (entry.isFile) {
      if (files.length >= MAX_DROP_FILES) {
        throw new DropRefused(
          `This drop holds more than ${MAX_DROP_FILES} files, which is past what a browser upload should carry. The cockpit on one of your machines is the right tool for a folder that size.`,
        );
      }
      files.push({ file: await fileOf(entry), dirPath });
      return;
    }
    if (entry.isDirectory) {
      const children = await allEntries(entry);
      for (const child of children) {
        await walk(child, [...dirPath, entry.name], depth + 1);
      }
    }
  }

  try {
    for (const entry of entries) {
      await walk(entry, [], 0);
    }
  } catch (err) {
    if (err instanceof DropRefused) return { files: [], refusal: err.message };
    throw err;
  }
  return { files, refusal: "" };
}

/** The entries of a drop, where the platform offers them. Empty on browsers
 *  without directory support -- the caller falls back to dataTransfer.files. */
export function entriesOf(dataTransfer: DataTransfer): EntryLike[] {
  const out: EntryLike[] = [];
  for (const item of Array.from(dataTransfer.items ?? [])) {
    const getter = (item as { webkitGetAsEntry?: () => unknown }).webkitGetAsEntry;
    const entry = typeof getter === "function" ? (getter.call(item) as EntryLike | null) : null;
    if (entry) out.push(entry);
  }
  return out;
}

/** Whether a drop carries at least one DIRECTORY -- what routes it through
 *  the folder flow rather than the plain file path. */
export function hasDirectory(entries: readonly EntryLike[]): boolean {
  return entries.some((e) => e.isDirectory);
}
