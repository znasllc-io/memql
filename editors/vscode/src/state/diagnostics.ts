// The information policy's display primitives (memql#4194).
//
// The extension's rule -- written down in README.md's Security section -- is
// that PANELS, TOASTS and TOOLTIPS carry a short, classified verdict, and the
// raw material (transport errors, capability stderr, exception detail) lives in
// an OUTPUT CHANNEL where it can be scrolled, copied and redacted before it is
// shown at all. These helpers are the mechanical half of that rule: every
// surface that shortens a message or records the long form does it through
// here, so the policy has one implementation rather than a convention per
// call site.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the adapters hand in an OutputChannel through the structural DiagnosticSink,
// and everything here stays unit-testable under bare `node --test`.

/**
 * The one method these helpers need from an OutputChannel.
 *
 * A structural subset rather than the vscode type, so pure modules and tests
 * can satisfy it with a plain object.
 */
export interface DiagnosticSink {
  appendLine(line: string): void;
}

/** A sink that drops everything -- the default where no channel was wired. */
export const NULL_SINK: DiagnosticSink = { appendLine: () => {} };

/**
 * The short form of a raw error for a toast or a tooltip.
 *
 * FIRST LINE ONLY, then capped. A transport failure's message routinely carries
 * a stack fragment, a URL and an OS error string; the surface this feeds exists
 * to say WHICH KIND of thing went wrong, and the full text belongs in the
 * channel (see recordDiagnostic). The cap is generous enough for every
 * classified sentence this codebase composes and tight enough that nothing
 * resembling a dump fits.
 */
export function briefMessage(raw: string, max = 140): string {
  const firstLine = raw.split("\n", 1)[0]?.trim() ?? "";
  if (firstLine.length <= max) return firstLine;
  return `${firstLine.slice(0, max - 3).trimEnd()}...`;
}

/**
 * Appends one dated record to a channel: headline, optional detail block, blank
 * separator. The same shape `writeTraining` established for the MemQL Training
 * channel, so the three channels read alike.
 *
 * `nowIso` is a parameter rather than a `new Date()` here so the record's
 * timestamp is testable; adapters pass `new Date().toISOString()`.
 */
export function recordDiagnostic(
  sink: DiagnosticSink,
  headline: string,
  detail: string,
  nowIso: string,
): void {
  sink.appendLine(`[${nowIso}] ${headline}`);
  if (detail.trim() !== "" && detail.trim() !== headline.trim()) {
    sink.appendLine(detail.trimEnd());
  }
  sink.appendLine("");
}
