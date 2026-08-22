// What the recorded checkout is, right now: commit, ref, dirtiness.
//
// READ WITH GIT AT THE MOMENT IT MATTERS -- the rebuild preflight -- and never
// from the receipt. The receipt says what was CLONED, which is the right answer
// for a repair replaying an install; this says what is THERE TODAY, which is
// the only honest answer in front of a build. A developer who checked out
// another branch, or who has four uncommitted files, is about to build images
// from that, and the checklist has to name it before the build rather than
// after the cluster is serving it.
//
// SAME SPAWN DISCIPLINE AS tags.ts: no shell, cwd set to the checkout, a
// timeout, and `undefined` rather than an exception when git cannot answer. A
// checklist is not a place to throw -- an unreadable checkout is a LINE on it
// ("git could not read the checkout"), which is a different sentence from
// "clean at 0000000" and must not be able to be rendered as one.
//
// Every decision lives in `parseCheckoutState`, which is pure and takes the
// four command outputs as strings; the spawn around it decides nothing.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4246

import { spawn } from "node:child_process";

/** How long each git read is given before the checkout is reported unreadable. */
export const CHECKOUT_READ_TIMEOUT_MS = 10_000;

export interface CheckoutState {
  commit: string;
  /**
   * What the checkout is ON, preferring the most specific answer.
   *
   * A tag beats a branch because a wizard install produces a DETACHED checkout
   * at a release tag: the tag is the thing an operator recognises, and the
   * branch read returns nothing there anyway. `detached` with an empty name is
   * the honest third answer, not a failure -- a commit checked out directly is
   * an ordinary state for a developer bisecting.
   */
  ref: { kind: "tag" | "branch" | "detached"; name: string };
  dirtyCount: number;
  /** Whether anything under deploy/ is modified -- manifests do not ride a rebuild. */
  deployDirty: boolean;
}

/**
 * The four git outputs, as a state.
 *
 * `git status --porcelain` writes one line per changed path in `XY PATH` form,
 * so the path begins at column 3 -- `slice(3)` rather than a substring search,
 * because `editors/vscode/deploy/x.ts` contains `deploy/` and is not it.
 */
export function parseCheckoutState(raw: {
  head: string;
  tag: string;
  branch: string;
  status: string;
}): CheckoutState {
  const lines = raw.status.split("\n").filter((l) => l.trim() !== "");
  const tag = raw.tag.trim();
  const branch = raw.branch.trim();
  return {
    commit: raw.head.trim(),
    ref:
      tag !== ""
        ? { kind: "tag", name: tag }
        : branch !== ""
          ? { kind: "branch", name: branch }
          : { kind: "detached", name: "" },
    dirtyCount: lines.length,
    deployDirty: lines.some((l) => l.slice(3).startsWith("deploy/")),
  };
}

/** One git invocation in the checkout, resolving its stdout. Injected by tests. */
type Run = (args: string[]) => Promise<string>;

/**
 * The checkout's state, or `undefined` when git cannot answer at all.
 *
 * THE TWO `catch(() => "")`s ARE NOT SLOPPINESS. `git describe --exact-match`
 * exits non-zero on a commit carrying no tag, and `git symbolic-ref -q` exits
 * non-zero on a detached HEAD -- both are the COMMON case rather than a
 * failure, and treating either as "git is unavailable" would throw away the
 * branch name and the dirt count with it. The two reads that must succeed are
 * `rev-parse HEAD` and `status --porcelain`: a directory where those fail is
 * not a checkout this editor can say anything about.
 */
export async function readCheckoutState(
  dir: string,
  run: Run = (args) => git(dir, args),
): Promise<CheckoutState | undefined> {
  try {
    const head = await run(["rev-parse", "HEAD"]);
    const tag = await run(["describe", "--exact-match", "--tags", "HEAD"]).catch(() => "");
    const branch = await run(["symbolic-ref", "--short", "-q", "HEAD"]).catch(() => "");
    const status = await run(["status", "--porcelain"]);
    return parseCheckoutState({ head, tag, branch, status });
  } catch {
    return undefined;
  }
}

function git(cwd: string, args: string[], timeoutMs = CHECKOUT_READ_TIMEOUT_MS): Promise<string> {
  return new Promise((resolve, reject) => {
    let child;
    try {
      // `shell: false`, as tags.ts spells it: the directory is an operator's
      // own path and a shell would give it a chance to be more than a path.
      child = spawn("git", args, { cwd, stdio: ["ignore", "pipe", "pipe"], shell: false });
    } catch (err) {
      reject(err instanceof Error ? err : new Error(String(err)));
      return;
    }
    let out = "";
    let err = "";
    const timer = setTimeout(() => child.kill(), timeoutMs);
    child.stdout?.on("data", (chunk: Buffer) => {
      out += chunk.toString("utf8");
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      err += chunk.toString("utf8");
    });
    child.on("error", (e: Error) => {
      clearTimeout(timer);
      reject(e);
    });
    child.on("close", (code: number | null) => {
      clearTimeout(timer);
      if (code === 0) resolve(out);
      else reject(new Error(err.trim() || `git ${args[0] ?? ""} exited ${String(code)}`));
    });
  });
}
