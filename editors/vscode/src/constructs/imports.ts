// The TypeScript half of the `memql/imports` contract (memql#3335).
//
// THE LANGUAGE SERVER OWNS ALL .memql PARSING. This module exists to make that
// true of the one place it was not: run/bundle.ts walked the import graph with
// a line regex, because there was no import request to ask. There is now.
//
// A regex cannot know what the lexer knows. A `use` line inside a /* ... */
// block comment is not an import, and the scan followed it anyway; the failure
// in the other direction -- a dirty dependency missed, so the run executes the
// SAVED copy of a file the developer is editing -- is the invisible class this
// epic kept tripping over.
//
// The wire shape below is a FIXED contract mirrored from cmd/memql-lsp/imports.go.
// Field names and the "empty array, never null" rule on `imports` and `names`
// are load-bearing; changing either means changing both sides in one commit.
//
// Deliberately free of `vscode` imports so it is unit-testable under bare
// `node --test`; extension.ts is the adapter that sends the request. The rule
// is enforced mechanically -- see cmd/memql-lsp/vscodeimportrule_test.go.

// -----------------------------------------------------------------------------
// The wire contract
// -----------------------------------------------------------------------------

export interface MemqlImport {
  /**
   * The dotted MODULE path as written -- "cognition.concepts".
   *
   * It names a FILE (dsl/cognition/concepts.memql), not a construct.
   * Resolving it onto disk stays on this side: the workspace layout being
   * walked is the client's.
   */
  path: string;
  /**
   * The imported-construct list from the brace group, in authored order.
   *
   * The bundle walk does not use it -- a file with any unsaved edit goes into
   * the bundle whole, whichever of its constructs are referenced -- but it is
   * what the server has, and dropping it here would mean a second round trip
   * for the first consumer that wants it.
   */
  names: string[];
}

// The server capability key. Feature-detect on this rather than calling the
// request blind and handling MethodNotFound -- an older memql-lsp on the PATH
// is an ordinary situation (serverPath is a user setting and the PATH fallback
// is whatever is installed), not an error worth a popup mid-run.
export const IMPORTS_CAPABILITY = "memqlImports";
export const IMPORTS_METHOD = "memql/imports";

// -----------------------------------------------------------------------------
// Defensive decoding
// -----------------------------------------------------------------------------

/**
 * parseImports narrows the raw JSON-RPC result.
 *
 * The language server is a separate process, possibly an OLDER BUILD than this
 * extension, so the reply is untrusted in the ordinary sense. Every
 * malformation degrades to "this is not an import" rather than throwing: a
 * thrown error here would abort a run the developer just started, and the
 * degraded outcome -- a dependency left out of the bundle -- is the same
 * bounded cost the walk has always had for an import it could not resolve.
 */
export function parseImports(raw: unknown): MemqlImport[] {
  if (raw === null || typeof raw !== "object") return [];
  const imports = (raw as { imports?: unknown }).imports;
  if (!Array.isArray(imports)) return [];
  const out: MemqlImport[] = [];
  for (const entry of imports) {
    const parsed = parseOne(entry);
    if (parsed !== undefined) out.push(parsed);
  }
  return out;
}

/**
 * importPaths is the bundle walk's view: the distinct module paths, in first
 * appearance order.
 *
 * Deduplicated HERE rather than on the server, which reports declarations as
 * authored. Two `use` lines naming one module are two declarations and one
 * file, and the walk wants the file.
 */
export function importPaths(imports: readonly MemqlImport[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const imp of imports) {
    if (seen.has(imp.path)) continue;
    seen.add(imp.path);
    out.push(imp.path);
  }
  return out;
}

function parseOne(raw: unknown): MemqlImport | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  const path = r.path;
  if (typeof path !== "string" || path === "") return undefined;
  // The contract promises an array, but a missing or malformed `names` says
  // nothing about whether the module is imported -- and the walk does not read
  // names at all. Degrading to [] keeps the dependency rather than dropping it
  // over a field this consumer does not use.
  const names = Array.isArray(r.names)
    ? r.names.filter((n): n is string => typeof n === "string")
    : [];
  return { path, names };
}
