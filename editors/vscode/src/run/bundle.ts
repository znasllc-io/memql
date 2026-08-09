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
// ON PARSING: this module scans for `use` lines to walk the IMPORT GRAPH. That
// is not a construct parse and does not encroach on the language server's
// ownership of .memql syntax -- it never looks inside a construct body, never
// derives an argument, and never decides what is runnable. It cannot go
// through the LSP because there is no import-graph request to ask, and adding
// one is out of scope for B2. If the scan mis-reads a line the cost is bounded
// and visible: a dirty dependency is missed (the run uses the deployed version
// of it, exactly as if the file were saved-and-not-deployed) or a file is
// included that need not have been (the sandbox compiles it and says so).
//
// Deliberately free of `vscode` imports; the adapter supplies file access
// through WorkspaceSources so this stays testable under bare `node --test`.

// Matches a file-top import line and captures the dotted path:
//
//     use cognition.concepts.{ participant, space }
//     use common.traits.{ isActiveRecord }
//     use cognition.shapes.{
//       participantFull
//     }
//
// The `.{` is required, which is what makes this a scan for a known statement
// rather than a guess: the brace list is the only import form the loader
// accepts (the legacy `@use*` annotation family is retired and rejected at
// parse time), so a `use` line without one is not an import this walk should
// follow. Anchored to line start with only leading whitespace allowed, so the
// word `use` inside a string or a comment body cannot match.
const USE_IMPORT_RE = /^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\.\{/;

/**
 * parseUseImports returns the dotted import paths a source file declares, in
 * declaration order, deduplicated.
 *
 * A dotted path names a FILE, not a construct: `cognition.shapes` is
 * `dsl/cognition/shapes.memql`. The brace list names constructs, and this walk
 * does not care which -- a file with any unsaved edit goes into the bundle
 * whole.
 */
export function parseUseImports(source: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const line of source.split("\n")) {
    const m = USE_IMPORT_RE.exec(line);
    if (m === null) continue;
    const dotted = m[1];
    if (dotted === undefined || seen.has(dotted)) continue;
    seen.add(dotted);
    out.push(dotted);
  }
  return out;
}

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
export function assembleBundle(
  activePath: string,
  activeText: string,
  ws: WorkspaceSources,
): Bundle {
  const dirtyDeps: Array<{ path: string; text: string }> = [];

  // Breadth-first over the import graph. `visited` is seeded with the active
  // file so a dependency cycle -- or a file that imports its own namespace --
  // cannot enqueue it a second time and duplicate every construct in it.
  const visited = new Set<string>([activePath]);
  const queue: string[] = [];
  for (const dotted of parseUseImports(activeText)) {
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
    for (const dotted of parseUseImports(source.text)) {
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
