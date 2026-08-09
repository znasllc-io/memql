// Assembling the .memql bundle a run is validated and session-defined from.
//
// The rule, from the runtime-panel design's "The run loop": the bundle is the
// ACTIVE FILE, plus any transitively `use`-imported workspace file that
// currently has UNSAVED EDITS. Everything else resolves against the live
// registry.
//
// That "everything else" half is what keeps the bundle small and the semantics
// honest. A session-defined construct never shadows core, so shipping the
// whole dependency tree would not make the run more faithful -- it would just
// mean more constructs the sandbox has to compile, more diagnostics to map
// back, and more chances for an unrelated file to fail the run. A file the
// developer has not touched is, by definition, the file the cluster already
// has.
//
// ON PARSING: THE LANGUAGE SERVER OWNS IT. This module walks the IMPORT GRAPH
// by asking `memql/imports` (memql#3335) which files a buffer imports; it does
// not look at .memql syntax at all.
//
// It used to scan for `use <dotted>.{` with a line regex, on the reasoning
// that an import-graph walk is not a construct parse -- it never reads a body,
// never derives an argument, never decides what is runnable -- and that a
// mis-read was bounded and visible either way. That was true and still not
// good enough. A regex cannot know what the lexer knows: a `use` line inside a
// /* ... */ block comment is not an import, and the scan followed it anyway.
// And the failure it produces in the other direction is precisely the
// invisible class this epic kept tripping over -- a missed dirty dependency
// means the run executes the SAVED copy of a file the developer is looking at,
// with nothing on screen to say so.
//
// So the walk asks the compiler's own lexer, through the server, exactly as
// the arg form asks it what a construct accepts. One parser.
//
// ASYNC, where the old scan was synchronous. Each hop is a request now. The
// active buffer's text is still captured by the CALLER before any of this runs
// (it is passed in, not read back), so the window that synchrony was
// protecting -- the text changing between the decision to run and the bytes
// that get sent -- stays closed for the file that matters most.
//
// Deliberately free of `vscode` imports; the adapter supplies file access AND
// the import lookup through WorkspaceSources so this stays testable under bare
// `node --test`.

/** One file's current content, and whether the editor is holding unsaved edits for it. */
export interface BundleSource {
  text: string;
  dirty: boolean;
}

/**
 * The file-access surface the adapter supplies.
 *
 * `read` must answer for CLEAN files too, not only dirty ones. A dirty file
 * reached only THROUGH a clean one still belongs in the bundle, and the walk
 * cannot get there without reading the clean file's own import lines. The
 * adapter satisfies this by preferring an open document's live text and
 * falling back to the file on disk.
 */
export interface WorkspaceSources {
  /** Maps a dotted import path to an absolute file path, or undefined when it resolves outside the workspace. */
  resolveImport(dotted: string): string | undefined;
  /** Reads a file, or undefined when it does not exist / cannot be read. */
  read(path: string): BundleSource | undefined;
  /**
   * The dotted module paths `text` imports, from the LANGUAGE SERVER.
   *
   * `path` identifies the document; `text` is the content to analyse -- the
   * caller's view of the file, which is the one the bundle is assembled from.
   * They are passed together because the walk reaches files the editor has
   * never opened (a CLEAN file is a legitimate route to a DIRTY one), so the
   * server cannot be expected to hold a copy.
   *
   * Returning `[]` is the graceful degradation when the server does not
   * advertise the request -- an older memql-lsp on the PATH is an ordinary
   * situation, not an error. The bundle then carries the active file alone,
   * which means a dirty dependency runs as its deployed version: the same
   * bounded, pre-existing cost a mis-read line used to have, and never a
   * second parser reintroduced as a fallback.
   */
  imports(path: string, text: string): Promise<string[]> | string[];
}

/**
 * One file's slice of the assembled bundle.
 *
 * `startLine` is the 0-based line index at which this file's first line sits
 * INSIDE the bundle string. It is the whole point of this structure: the
 * engine reports diagnostics in bundle-file coordinates, and without the
 * offset table there is no way back to a buffer position.
 *
 * `lines` is retained (rather than recomputed from the bundle) so the
 * diagnostic mapper can widen a start-only position to end-of-line without a
 * second split.
 */
export interface BundleFile {
  path: string;
  startLine: number;
  lines: string[];
}

export interface Bundle {
  /** The concatenated .memql source submitted to validate / session-define. */
  sources: string;
  /**
   * The files in bundle order -- dirty dependencies first, ACTIVE FILE LAST.
   * The mapper relies on that last position for its fallback file, so the
   * order is contract, not presentation.
   */
  files: BundleFile[];
}

/**
 * assembleBundle builds the bundle for a run rooted at `activePath`.
 *
 * `activeText` is passed separately from `ws.read` on purpose: the active
 * buffer's CURRENT text is the entire point of the exercise, and reading it
 * back through the same path as everything else invites a caller to hand over
 * a stale or on-disk copy. The active file is always included regardless of
 * whether it is dirty -- a saved-but-not-deployed buffer still has to be
 * injected, or the run silently executes the deployed construct.
 */
export async function assembleBundle(
  activePath: string,
  activeText: string,
  ws: WorkspaceSources,
): Promise<Bundle> {
  const dirtyDeps: Array<{ path: string; text: string }> = [];

  // Breadth-first over the import graph. `visited` is seeded with the active
  // file so a dependency cycle -- or a file that imports its own namespace --
  // cannot enqueue it a second time and duplicate every construct in it.
  const visited = new Set<string>([activePath]);
  const queue: string[] = [];
  for (const dotted of await ws.imports(activePath, activeText)) {
    const resolved = ws.resolveImport(dotted);
    if (resolved !== undefined) queue.push(resolved);
  }

  while (queue.length > 0) {
    const path = queue.shift() as string;
    if (visited.has(path)) continue;
    visited.add(path);
    const source = ws.read(path);
    if (source === undefined) continue;
    if (source.dirty) dirtyDeps.push({ path, text: source.text });
    // Traversed whether or not this file is dirty: a clean file is a legitimate
    // route to a dirty one, and stopping at it would silently run the deployed
    // version of an edit the developer is looking at.
    for (const dotted of await ws.imports(path, source.text)) {
      const resolved = ws.resolveImport(dotted);
      if (resolved !== undefined && !visited.has(resolved)) queue.push(resolved);
    }
  }

  const ordered = [...dirtyDeps, { path: activePath, text: activeText }];

  const files: BundleFile[] = [];
  const chunks: string[] = [];
  let startLine = 0;
  for (const entry of ordered) {
    // Normalised to exactly one trailing newline so a file that does not end
    // in one cannot glue its last line onto the next file's first -- which
    // would both corrupt the source and shift every subsequent offset by one.
    const normalised = entry.text.endsWith("\n") ? entry.text : `${entry.text}\n`;
    const lines = normalised.slice(0, -1).split("\n");
    files.push({ path: entry.path, startLine, lines });
    chunks.push(normalised);
    startLine += lines.length;
  }

  return { sources: chunks.join(""), files };
}

/**
 * bundleFileAt resolves a 0-based BUNDLE line index to the file containing it.
 *
 * Returns undefined for a line past the end of the bundle, which the caller
 * has to treat as "no position" rather than clamping -- a diagnostic pointing
 * past the source it was computed from means the offset table and the engine
 * disagree, and inventing a line inside the last file would hide that.
 */
export function bundleFileAt(bundle: Bundle, bundleLine: number): BundleFile | undefined {
  if (bundleLine < 0) return undefined;
  for (const f of bundle.files) {
    if (bundleLine >= f.startLine && bundleLine < f.startLine + f.lines.length) return f;
  }
  return undefined;
}
