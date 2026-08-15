// Assembling what a training action actually submits.
//
// THE BUNDLE IS THE CLOSURE, NOT THE CONSTRUCT. A promote carries the construct
// AND WHAT IT DEPENDS ON: promoting a query whose spec the cluster has never
// seen would put a construct into the shared registry that cannot bind, so the
// dependency travels with it or neither goes.
//
// THE RULE HERE IS NOT run/bundle.ts's RULE, and the difference is the whole
// reason this module exists rather than reusing that one. The run bundle carries
// the active file plus any transitively imported workspace file WITH UNSAVED
// EDITS, on the stated reasoning that "a file the developer has not touched is,
// by definition, the file the cluster already has." That reasoning is exactly
// false for a promote: an UNTRAINED dependency is one the cluster does not have,
// saved or not, and a saved file is the ordinary case for it rather than the
// exception. So the inclusion test is the per-construct TRAINING STATE, and this
// module cannot even see dirtiness -- `TrainingWorkspace.read` returns text and
// no `dirty` flag, so the run path's rule is not merely unused here, it is
// unreachable.
//
//   untrained / drifted  -> the cluster does not have this version. IN.
//   trained / seeded     -> the cluster already has it. OUT.
//
// The unit of INCLUSION is the file, because the engine compiles source text and
// a construct's `use` imports resolve at file scope; the unit of DECISION is the
// construct, because that is what has a state. A file joins the bundle when it
// declares at least one construct in a state the cluster does not have.
//
// WHAT HAPPENS WHEN THE STATE CANNOT BE ESTABLISHED is the judgement call, and
// it goes to EXCLUDE-AND-SAY-SO. Both directions fail loudly -- an omitted
// untrained dependency fails Gate-1 with a bind error naming it, an included
// seeded one is refused for shadowing a core name -- so the choice is about
// which failure is more useful and which default is right more often. A file the
// walk reaches and cannot classify is overwhelmingly a core `dsl/` file the
// cluster has had since boot; including those would refuse most promotes
// outright. So it is left out, and the closure REPORTS it as unclassified rather
// than silently dropping it.
//
// ON PARSING: THE LANGUAGE SERVER OWNS IT, exactly as run/bundle.ts states. The
// import graph comes from `memql/imports` and the construct boundaries come from
// `memql/trainingState`'s signature ranges. Nothing here reads .memql syntax --
// with one bounded exception in `constructWindow` below, which trims trailing
// annotation lines off a slice and says so.
//
// Deliberately free of `vscode` imports; the adapter in extension.ts supplies
// file access, the import lookup and the training-state lookup. Tested under
// bare `node --test`.
//
// Refs: #3763 #3745

import type { Bundle, BundleFile } from "../run/bundle.js";
import type { TrainingConstruct, TrainingState } from "../state/training.js";

/**
 * The workspace surface a closure walk needs.
 *
 * `resolveImport` and `imports` are run/bundle.ts's, unchanged -- one import
 * graph, walked the same way. `read` deliberately is NOT: it hands back text and
 * nothing else, because the run path's dirty flag has no meaning here and a
 * field that exists is a field somebody eventually branches on.
 */
export interface TrainingWorkspace {
  /** Maps a dotted import path to an absolute file path, or undefined when it resolves outside the workspace. */
  resolveImport(dotted: string): string | undefined;
  /** Reads a file, or undefined when it does not exist / cannot be read. */
  read(path: string): { text: string } | undefined;
  /** The dotted module paths `text` imports, from the LANGUAGE SERVER. `[]` when it cannot say. */
  imports(path: string, text: string): Promise<string[]> | string[];
  /**
   * The per-construct training state of a file, from the LANGUAGE SERVER.
   *
   * `undefined` means the server could not answer -- which is NOT the same as
   * `[]` (a file that declares nothing). The two get different closure
   * decisions, so they stay distinguishable all the way from the wire, the same
   * null-is-not-empty rule the cluster catalog push turns on.
   *
   * Takes the PATH and not the text, unlike `imports`. `memql/trainingState`
   * answers from the server's own copy of the document and takes no text
   * parameter, so handing it any would create a second answer to "which bytes
   * were classified" that nothing could reconcile. The adapter's job is to make
   * sure the server holds the document before asking.
   */
  trainingStates(path: string): Promise<TrainingConstruct[] | undefined> | TrainingConstruct[] | undefined;
}

/** Why a file is in the bundle, or why it is not. */
export type ClosureReason =
  /** The file the action was invoked from. Always in, whatever its state. */
  | "active"
  /** Declares at least one untrained or drifted construct: the cluster does not have this version. */
  | "untrained"
  /** Every construct it declares is already on the cluster. */
  | "onCluster"
  /** The language server could not say. Left out; see the module comment. */
  | "unclassified";

/** One file's place in the closure, with the constructs that decided it. */
export interface ClosureMember {
  path: string;
  included: boolean;
  reason: ClosureReason;
  /**
   * The constructs the server reported for this file, in declaration order.
   * Empty for an unclassified file -- there is nothing to report, which is
   * exactly what "unclassified" means.
   */
  constructs: ClosureConstruct[];
}

export interface ClosureConstruct {
  kind: string;
  name: string;
  state: TrainingState;
}

/**
 * A bundle plus the account of how it was chosen.
 *
 * It IS a `Bundle`, so `mapBundleDiagnostics` maps its diagnostics back to
 * buffer coordinates with no adaptation at all -- the offset table is the same
 * structure the run path builds, because the engine reports positions against
 * the concatenated string either way.
 */
export interface TrainingBundle extends Bundle {
  /** Every file the walk reached, INCLUDED OR NOT, in walk order with the active file first. */
  members: ClosureMember[];
}

/**
 * assembleClosure builds the bundle a dry-run, a try-in-session or a promote
 * submits.
 *
 * `activeText` is passed separately from `ws.read` for run/bundle.ts's reason:
 * the active buffer's CURRENT text is the point of the exercise, and reading it
 * back through the same path as everything else invites a caller to hand over a
 * stale copy.
 */
export async function assembleClosure(
  activePath: string,
  activeText: string,
  ws: TrainingWorkspace,
): Promise<TrainingBundle> {
  const active: ClosureMember = {
    path: activePath,
    included: true,
    reason: "active",
    constructs: describe(await ws.trainingStates(activePath)),
  };
  const members: ClosureMember[] = [active];
  const included: Array<{ path: string; text: string }> = [];

  // Breadth-first over the import graph, seeded with the active file so a
  // dependency cycle -- or a file importing its own namespace -- cannot enqueue
  // it again and duplicate every construct in it.
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

    const states = await ws.trainingStates(path);
    const member = classify(path, states);
    members.push(member);
    if (member.included) included.push({ path, text: source.text });

    // Traversed whether or not this file joins the bundle. A file the cluster
    // already has is a legitimate route to one it does not, and stopping at it
    // would leave the dependency out of a promote that needs it -- the exact
    // half-landing this closure exists to prevent.
    for (const dotted of await ws.imports(path, source.text)) {
      const resolved = ws.resolveImport(dotted);
      if (resolved !== undefined && !visited.has(resolved)) queue.push(resolved);
    }
  }

  // ACTIVE FILE LAST in bundle order: mapBundleDiagnostics uses the last file as
  // the home for a positionless diagnostic, and the active file is the one the
  // developer will look at. Contract, not presentation.
  const { sources, files } = concatenate([...included, { path: activePath, text: activeText }]);
  return { sources, files, members };
}

/**
 * assembleConstruct builds the bundle a DEMOTE submits: one construct, alone.
 *
 * A demote is not the inverse of the closure and must not be. Promote carries a
 * construct's dependencies because they have to arrive with it; demote carries
 * only the construct itself, because withdrawing a dependency other promoted
 * constructs still bind against would break them -- and nothing about clicking
 * Demote on one construct asks for that.
 *
 * The engine demotes BY NAME: `DemoteBundleDurable` splits the source into
 * constructs and withdraws every demotable one it finds, never compiling any of
 * them. So the whole requirement on this source is that it contains the target's
 * declaration and NO OTHER construct's.
 *
 * It is built by BLANKING, not by cutting. Every line outside the construct's
 * window is replaced with an empty line, so the source keeps the file's exact
 * line numbering while declaring exactly one construct. Cutting would have
 * produced the same names against shifted coordinates, which is the kind of
 * off-by-N that only shows up in whatever the engine eventually says about a
 * line.
 *
 * Returns undefined when the file declares no construct by that name -- the
 * buffer changed under a lens, which is an ordinary race and not an error.
 */
export function assembleConstruct(
  activePath: string,
  activeText: string,
  constructs: readonly TrainingConstruct[],
  name: string,
): TrainingBundle | undefined {
  const window = constructWindow(activeText, constructs, name);
  if (window === undefined) return undefined;

  const lines = splitLines(activeText);
  const kept = lines.map((line, index) =>
    index >= window.start && index < window.end ? line : "",
  );
  const target = constructs.find((c) => c.name === name);
  const sources = `${kept.join("\n")}\n`;
  return {
    sources,
    files: [{ path: activePath, startLine: 0, lines: kept }],
    members: [
      {
        path: activePath,
        included: true,
        reason: "active",
        constructs:
          target === undefined
            ? []
            : [{ kind: target.kind, name: target.name, state: target.state }],
      },
    ],
  };
}

/**
 * constructWindow is the half-open line range `name`'s declaration occupies.
 *
 * The START is the server's own signature range -- no syntax is read to find it.
 * The END is the next construct's signature start, which is where this
 * construct's text provably stops, walked back over the trailing blank, comment
 * and annotation lines that belong to the NEXT declaration rather than to this
 * one.
 *
 * That walk-back is the one lexical judgement in this module, and its failure
 * mode is bounded in both directions. Trimming too little leaves an annotation
 * line in the source, which the engine's splitter classifies as an unrecognized
 * region and the demote's demotable-kinds filter drops. Trimming too much cannot
 * reach the construct's body, because a body ends in `}` and `}` is none of the
 * three things the walk-back trims.
 *
 * Exported for the test that pins both edges.
 */
export function constructWindow(
  text: string,
  constructs: readonly TrainingConstruct[],
  name: string,
): { start: number; end: number } | undefined {
  const lines = splitLines(text);
  // Sorted by position rather than trusting declaration order: the range is what
  // decides adjacency, and a server that ever returned constructs in another
  // order would otherwise produce a window running backwards.
  const ordered = [...constructs].sort(
    (a, b) => a.signatureRange.start.line - b.signatureRange.start.line,
  );
  // FIRST match by name. The lens hands over `{uri, name}` and nothing else, so
  // a file declaring the same name twice under different kinds is ambiguous at
  // the call site rather than here; the engine then demotes the kind this slice
  // actually contains, which is the one the developer's cursor was next to.
  const index = ordered.findIndex((c) => c.name === name);
  if (index < 0) return undefined;

  const start = ordered[index]!.signatureRange.start.line;
  if (start < 0 || start >= lines.length) return undefined;

  const next = ordered[index + 1];
  let end = next === undefined ? lines.length : next.signatureRange.start.line;
  if (end > lines.length) end = lines.length;
  while (end > start + 1 && isBetweenConstructs(lines[end - 1] ?? "")) end -= 1;
  return { start, end };
}

/** A line that can only sit BETWEEN declarations: blank, a comment, or an annotation. */
function isBetweenConstructs(line: string): boolean {
  const trimmed = line.trim();
  return trimmed === "" || trimmed.startsWith("//") || trimmed.startsWith("@");
}

/**
 * classify decides whether a dependency file joins the bundle.
 *
 * `undefined` and `[]` land in different buckets and that separation is
 * load-bearing: the first is the server declining to answer, the second is a
 * file that genuinely declares nothing. Only the first is worth telling the
 * developer about.
 */
function classify(path: string, states: TrainingConstruct[] | undefined): ClosureMember {
  if (states === undefined) {
    return { path, included: false, reason: "unclassified", constructs: [] };
  }
  const constructs = describe(states);
  const needed = constructs.some((c) => c.state === "untrained" || c.state === "drifted");
  return {
    path,
    included: needed,
    reason: needed ? "untrained" : "onCluster",
    constructs,
  };
}

function describe(states: TrainingConstruct[] | undefined): ClosureConstruct[] {
  if (states === undefined) return [];
  return states.map((c) => ({ kind: c.kind, name: c.name, state: c.state }));
}

/**
 * concatenate builds the bundle string and its offset table.
 *
 * Each file is normalised to exactly one trailing newline so a file that does
 * not end in one cannot glue its last line onto the next file's first -- which
 * would both corrupt the source and shift every subsequent offset by one.
 */
function concatenate(
  entries: readonly { path: string; text: string }[],
): { sources: string; files: BundleFile[] } {
  const files: BundleFile[] = [];
  const chunks: string[] = [];
  let startLine = 0;
  for (const entry of entries) {
    const normalised = entry.text.endsWith("\n") ? entry.text : `${entry.text}\n`;
    const lines = normalised.slice(0, -1).split("\n");
    files.push({ path: entry.path, startLine, lines });
    chunks.push(normalised);
    startLine += lines.length;
  }
  return { sources: chunks.join(""), files };
}

function splitLines(text: string): string[] {
  const normalised = text.endsWith("\n") ? text.slice(0, -1) : text;
  return normalised.split("\n");
}
