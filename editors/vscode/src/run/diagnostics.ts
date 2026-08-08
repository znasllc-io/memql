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
