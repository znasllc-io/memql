// Mapping engine authoring diagnostics back to buffer coordinates.
//
// The engine compiles a BUNDLE -- one concatenated string -- and reports each
// diagnostic against that string, 1-based. The developer is looking at files.
// This module is the only place the two coordinate systems meet.
//
// THE ZERO RULE. All four position fields are 0 when the engine could not
// compute a reliable position, and it deliberately emits no position rather
// than a wrong one (memql#2375). Zero therefore means "no position" and MUST
// NOT be read as line 0: doing so parks every positionless diagnostic on the
// first line of the first file in the bundle, which is a dirty dependency far
// more often than it is the file the developer is editing. A positionless
// diagnostic becomes a FILE-LEVEL diagnostic on the active file instead --
// visible in the Problems panel under the right filename, pointing at nothing
// in particular, which is exactly what the engine said.
//
// Deliberately free of `vscode` imports; views/webview adapters turn these
// into vscode.Diagnostic. Tested under bare `node --test`.

import type { AuthoringDiagnostic } from "@znasllc-io/memql-sdk-core/authoring";

import { bundleFileAt, type Bundle, type BundleFile } from "./bundle.js";

export interface DiagnosticPosition {
  /** 0-based, editor coordinates. */
  line: number;
  /** 0-based UTF-16 code-unit offset, editor coordinates. */
  character: number;
}

export interface MappedDiagnostic {
  /** Absolute path of the file this diagnostic belongs to. */
  path: string;
  /**
   * True when the engine reported no position. The range is then the whole
   * first line, and consumers should present it as belonging to the file
   * rather than to a location.
   */
  fileLevel: boolean;
  start: DiagnosticPosition;
  end: DiagnosticPosition;
  /** The engine's message, with the construct named. */
  message: string;
  /**
   * The failure's stable rule id (`lower_unknown_field`, ...), or "" when it
   * carries none (memql#5435). Rendered as the diagnostic's code, the same
   * field the language server's squiggles carry theirs in.
   */
  code: string;
  constructName: string;
  constructKind: string;
}

/**
 * mapBundleDiagnostics converts a validate/define result's diagnostics into
 * buffer-anchored ones.
 *
 * Only genuine FAILURES are mapped. A `skipped` construct reports ok=false
 * with skipped=true and does not fail the bundle -- it is the engine saying
 * "this pass does not compile shapes", not an error the developer can act on,
 * and rendering it in the Problems panel would put permanent noise under every
 * file that declares one.
 */
export function mapBundleDiagnostics(
  diagnostics: readonly AuthoringDiagnostic[],
  bundle: Bundle,
): MappedDiagnostic[] {
  const out: MappedDiagnostic[] = [];
  for (const d of diagnostics) {
    if (d.ok || d.skipped) continue;
    out.push(mapOne(d, bundle));
  }
  return out;
}

// fallbackFile is the ACTIVE file -- last in bundle order (see
// assembleBundle). A positionless diagnostic goes here rather than to the
// bundle's first file: the developer invoked the run from the active buffer,
// so that is the file whose Problems entry they will actually look at.
function fallbackFile(bundle: Bundle): BundleFile | undefined {
  return bundle.files[bundle.files.length - 1];
}

function mapOne(d: AuthoringDiagnostic, bundle: Bundle): MappedDiagnostic {
  const message = d.error === "" ? `${d.kind} ${d.name}: failed to compile` : d.error;

  // THE ZERO RULE, applied once, at the top. Everything below this branch has
  // a real 1-based line to work with.
  if (d.line <= 0) return fileLevel(d, bundle, message);

  const bundleLine = d.line - 1;
  const file = bundleFileAt(bundle, bundleLine);
  // A position past the end of the bundle means the offset table and the
  // engine disagree about the source that was submitted. Clamping into the
  // last file would invent a location; degrading to file-level says only what
  // is actually known.
  if (file === undefined) return fileLevel(d, bundle, message);

  const startLine = bundleLine - file.startLine;
  // Column 0 is the same "no position" sentinel applied to the horizontal
  // axis: the engine can know the line and not the column. Start of line is
  // the honest rendering.
  const startCharacter = d.column > 0 ? d.column - 1 : 0;

  const end = resolveEnd(d, file, startLine, startCharacter);

  return {
    path: file.path,
    fileLevel: false,
    start: { line: startLine, character: startCharacter },
    end,
    message: `${d.kind} ${d.name}: ${message}`,
    code: d.code,
    constructName: d.name,
    constructKind: d.kind,
  };
}

// resolveEnd widens a start-only position to something the editor can draw.
//
// An empty range renders as a zero-width caret with no squiggle, so a
// diagnostic with no end anchor -- the common case, since endLine and
// endColumn are 0 independently of line and column -- would be invisible in
// the editor while still occupying a Problems row. Extending to end-of-line is
// the conventional degradation and is why BundleFile retains its lines.
function resolveEnd(
  d: AuthoringDiagnostic,
  file: BundleFile,
  startLine: number,
  startCharacter: number,
): DiagnosticPosition {
  const endOfStartLine = (file.lines[startLine] ?? "").length;

  if (d.endLine <= 0) {
    return { line: startLine, character: Math.max(endOfStartLine, startCharacter) };
  }

  const endBundleLine = d.endLine - 1;
  const endLineInFile = endBundleLine - file.startLine;
  // An end anchor that fell outside THIS file's slice is not usable: the
  // range would span a file boundary that does not exist in the editor. Fall
  // back to end-of-start-line, which is inside the file by construction.
  if (endLineInFile < 0 || endLineInFile >= file.lines.length) {
    return { line: startLine, character: Math.max(endOfStartLine, startCharacter) };
  }
  if (d.endColumn <= 0) {
    return { line: endLineInFile, character: (file.lines[endLineInFile] ?? "").length };
  }
  const endCharacter = d.endColumn - 1;
  // A backwards range (end before start on the same line) is a contradiction
  // in the engine's own output; widening to end-of-line beats handing the
  // editor an inverted range it will silently normalise in some other way.
  if (endLineInFile === startLine && endCharacter <= startCharacter) {
    return { line: startLine, character: Math.max(endOfStartLine, startCharacter) };
  }
  return { line: endLineInFile, character: endCharacter };
}

function fileLevel(
  d: AuthoringDiagnostic,
  bundle: Bundle,
  message: string,
): MappedDiagnostic {
  const file = fallbackFile(bundle);
  const firstLineLength = (file?.lines[0] ?? "").length;
  return {
    path: file?.path ?? "",
    fileLevel: true,
    start: { line: 0, character: 0 },
    end: { line: 0, character: firstLineLength },
    // The message says the position is missing, so a reader is not left
    // wondering why a diagnostic about line 40's construct is sitting at the
    // top of the file.
    message: `${d.kind} ${d.name}: ${message} (the engine reported no source position for this failure)`,
    code: d.code,
    constructName: d.name,
    constructKind: d.kind,
  };
}

/** groupByFile buckets mapped diagnostics per file, for a per-URI diagnostic collection. */
export function groupByFile(
  diagnostics: readonly MappedDiagnostic[],
): Map<string, MappedDiagnostic[]> {
  const out = new Map<string, MappedDiagnostic[]>();
  for (const d of diagnostics) {
    const bucket = out.get(d.path);
    if (bucket === undefined) out.set(d.path, [d]);
    else bucket.push(d);
  }
  return out;
}

/**
 * The language server's diagnostic source -- `lsName` in cmd/memql-lsp. A
 * run's failures are compared against what that source draws.
 */
export const LANGUAGE_SERVER_SOURCE = "memql-lsp";

/** A diagnostic another source already draws: its rule id and its range. */
export interface ShownDiagnostic {
  code: string;
  start: DiagnosticPosition;
  end: DiagnosticPosition;
}

/**
 * dropAlreadyShown removes the run failures the language server already
 * draws in the same file: the same rule id over an overlapping range
 * (memql#5434). The server runs the load over the open buffer as the author
 * types, so a lowering refusal a run reports is usually on screen before the
 * run, and two squiggles for one fault is noise -- the server's, which stays
 * current as the buffer changes, is the one kept. A failure with no rule id,
 * or one the server does not draw (the cluster refused what the workspace
 * accepts), is kept.
 *
 * It answers for one moment. What the server draws changes after a run --
 * it clears a closed file, and drops a refusal whose line is being edited --
 * so a caller that filtered once would lose the failure for the rest of the
 * run. RunDiagnosticsView keeps the run's failures and asks again whenever
 * the server's diagnostics change.
 */
export function dropAlreadyShown(
  mapped: readonly MappedDiagnostic[],
  shown: readonly ShownDiagnostic[],
): MappedDiagnostic[] {
  return mapped.filter(
    (d) => d.code === "" || !shown.some((s) => s.code === d.code && rangesOverlap(d, s)),
  );
}

type Span = { start: DiagnosticPosition; end: DiagnosticPosition };

// rangesOverlap reports whether two ranges share a character; an empty range
// covers the one it starts at.
function rangesOverlap(a: Span, b: Span): boolean {
  const x = widenEmpty(a);
  const y = widenEmpty(b);
  return before(x.start, y.end) && before(y.start, x.end);
}

function widenEmpty(r: Span): Span {
  if (before(r.start, r.end)) return r;
  return { start: r.start, end: { line: r.start.line, character: r.start.character + 1 } };
}

function before(a: DiagnosticPosition, b: DiagnosticPosition): boolean {
  return a.line < b.line || (a.line === b.line && a.character < b.character);
}

/**
 * Where a RunDiagnosticsView draws: one file's diagnostics at a time, keyed by
 * absolute path. An empty list clears the file.
 */
export interface RunDiagnosticsSink {
  set(path: string, diagnostics: readonly MappedDiagnostic[]): void;
  clear(): void;
}

/**
 * RunDiagnosticsView holds a run's failures and draws each file's as
 * dropAlreadyShown leaves them against what the language server shows NOW.
 *
 * The filter is not applied once, at publish: the server's squiggle for a
 * fault can go away while the fault stays -- its file is closed, or an edit
 * on the line drops the refusal until the next load answers -- and a failure
 * filtered out against it would then be gone from the Problems panel for the
 * rest of the run. So the view keeps every failure and redraws a file
 * whenever the server's diagnostics for it change (refresh), which puts a
 * failure back when the server stops drawing it and takes it away when the
 * server starts.
 *
 * A file is redrawn only when what the server shows there has changed since it
 * was last drawn. That is also what keeps it from redrawing forever: drawing
 * changes the diagnostics of the file, which is an event the caller feeds back
 * to refresh, and the server's half of that file has not moved.
 */
export class RunDiagnosticsView {
  private failures = new Map<string, { path: string; diagnostics: MappedDiagnostic[] }>();
  private drawnAgainst = new Map<string, string>();

  /**
   * shownIn answers what the language server draws in a file now; keyOf
   * names a file the way refresh is told about it (the identity by default).
   */
  constructor(
    private readonly sink: RunDiagnosticsSink,
    private readonly shownIn: (path: string) => readonly ShownDiagnostic[],
    private readonly keyOf: (path: string) => string = (path) => path,
  ) {}

  /** publish replaces the run's failures and draws every file that has one. */
  publish(mapped: readonly MappedDiagnostic[]): void {
    this.failures.clear();
    this.drawnAgainst.clear();
    this.sink.clear();
    for (const [path, diagnostics] of groupByFile(mapped)) {
      if (path === "") continue;
      this.failures.set(this.keyOf(path), { path, diagnostics });
    }
    for (const key of this.failures.keys()) this.draw(key);
  }

  /**
   * refresh redraws the files, named by keyOf, whose language-server
   * diagnostics changed. A file with no failure, or whose server diagnostics
   * are as they were when it was last drawn, is left alone.
   */
  refresh(keys: readonly string[]): void {
    for (const key of keys) {
      if (this.failures.has(key)) this.draw(key);
    }
  }

  private draw(key: string): void {
    const entry = this.failures.get(key);
    if (entry === undefined) return;
    const shown = this.shownIn(entry.path);
    const against = shownKey(shown);
    if (this.drawnAgainst.get(key) === against) return;
    this.drawnAgainst.set(key, against);
    this.sink.set(entry.path, dropAlreadyShown(entry.diagnostics, shown));
  }
}

// shownKey names a set of shown diagnostics by everything dropAlreadyShown
// reads, in a fixed order.
function shownKey(shown: readonly ShownDiagnostic[]): string {
  return shown
    .map((d) => `${d.code}@${d.start.line}:${d.start.character}-${d.end.line}:${d.end.character}`)
    .sort()
    .join("|");
}
