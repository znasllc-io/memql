// A ~90-line sequential test harness for the Extension Development Host.
//
// WHY NOT MOCHA (the conventional @vscode/test-electron pairing): the fast
// lane in this package is bare `node --test` with zero test-framework
// dependencies, and the whole point of that choice is that `npm test` stays
// dependency-light. Pulling mocha + @vscode/test-cli in for six smoke cases
// would put a test framework into the extension's dev tree to do what a
// for-loop and `node:assert` already do. The one dependency this lane really
// cannot avoid is @vscode/test-electron itself, which downloads and drives a
// real VS Code.
//
// WHY NOT `node:test` INSIDE THE HOST: node:test's top-level `test()` has no
// awaitable "everything finished" handle and reports by writing TAP to
// stdout, which the extension host multiplexes with its own logging. This
// lane's contract is a PROMISE that rejects -- that is what makes
// @vscode/test-electron exit nonzero -- so the harness owns its own reporting.
//
// Cases run STRICTLY IN SEQUENCE and share one editor window. They are not
// independent: several open editors or webview tabs, and a parallel runner
// would have them fighting over the same window.

/** One registered smoke case. */
interface Case {
  name: string;
  fn: () => void | Promise<void>;
}

const cases: Case[] = [];

/** Registers a smoke case. Cases run in registration order. */
export function smoke(name: string, fn: () => void | Promise<void>): void {
  cases.push({ name, fn });
}

/**
 * SkipError aborts a case as skipped rather than failed.
 *
 * Used only where a precondition this lane deliberately does NOT provide is
 * missing -- a live cluster, above all. A skip is reported loudly (a SKIP
 * line, and a count in the summary) precisely so a lane that silently degraded
 * into asserting nothing is visible rather than green.
 */
export class SkipError extends Error {}

/** Aborts the current case as skipped, with a stated reason. */
export function skip(reason: string): never {
  throw new SkipError(reason);
}

/** Logs a note that is neither a pass nor a fail -- environment facts, mostly. */
export function info(message: string): void {
  console.log(`INFO: ${message}`);
}

/** Logs a condition worth a human's attention that is not a failure. */
export function warn(message: string): void {
  console.log(`WARNING: ${message}`);
}

/**
 * Runs every registered case in order and resolves only if none failed.
 *
 * A rejection here is what makes @vscode/test-electron exit nonzero, so the
 * failure summary is assembled into the rejection message rather than only
 * printed -- the CI log tail is the first thing anyone reads.
 */
export async function runCases(): Promise<void> {
  const failures: string[] = [];
  let passed = 0;
  let skipped = 0;

  for (const c of cases) {
    const started = Date.now();
    try {
      await c.fn();
      passed += 1;
      console.log(`SUCCESS: ${c.name} (${Date.now() - started}ms)`);
    } catch (err) {
      if (err instanceof SkipError) {
        skipped += 1;
        console.log(`SKIP: ${c.name} -- ${err.message}`);
        continue;
      }
      const detail = err instanceof Error ? (err.stack ?? err.message) : String(err);
      failures.push(`${c.name}: ${detail}`);
      console.log(`ERROR: ${c.name}`);
      console.log(detail);
    }
  }

  console.log(
    `\nhost smoke: ${passed} passed, ${failures.length} failed, ${skipped} skipped, ${cases.length} total`
  );
  if (failures.length > 0) {
    throw new Error(`host smoke lane: ${failures.length} case(s) failed\n\n${failures.join("\n\n")}`);
  }
}

/**
 * Waits for `check` to become true, polling until `timeoutMs` elapses.
 *
 * Host assertions are inherently eventual -- a file watcher fires when the
 * platform's watcher backend gets around to it, a view registers when the
 * workbench finishes contributing it. A fixed sleep would either be flaky or
 * slow; this is neither.
 */
export async function waitFor(
  what: string,
  check: () => boolean | Promise<boolean>,
  timeoutMs = 15_000,
  intervalMs = 100
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await check()) {
      return;
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out after ${timeoutMs}ms waiting for ${what}`);
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}
