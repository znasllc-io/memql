// What an update would find, right now, without doing one.
//
// READ WITH GIT AT THE MOMENT IT MATTERS -- the update preflight -- for the
// same reason checkoutState.ts is (memql#4246): the receipt says what was
// CLONED, and a developer who has been working in that directory for a week is
// about to update something quite different.
//
// WHAT IT DELIBERATELY DOES NOT DO IS FETCH. A checklist runs on a click and a
// fetch over a slow link -- or worse, the one-time deepening of a `--depth 1`
// clone -- is a screen that sits blank. So the remote is read with
// `ls-remote`, which asks for one line and no objects, and the two counts are
// answered EXACTLY when the remote's commit already happens to be in the local
// object store and reported as UNKNOWN otherwise.
//
// Unknown is a real answer here and is rendered as one. The alternative --
// computing the counts against the stale remote-tracking ref -- is a specific,
// confident number that was true at the last fetch, which is the shape of wrong
// answer this whole area of the extension exists to avoid.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4577 #4578 #4573

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";

/** How long each git read is given before the checkout is reported unreadable. */
export const UPDATE_READ_TIMEOUT_MS = 10_000;

/** How long the one REMOTE read is given. It is a network call, so it gets more. */
export const UPDATE_REMOTE_TIMEOUT_MS = 20_000;

/** An unfinished git operation, named as an operator would say it. */
export type InProgress = "" | "a merge" | "a rebase" | "a cherry-pick" | "a revert";

export interface UpdateState {
  /** The commit the checkout is on. */
  head: string;
  /**
   * The branch the update would move to.
   *
   * EMPTY IS BLOCKING, and it is reachable: a release install checks out a tag
   * detached, and a repair of a branch install reconciles onto an exact commit
   * and detaches too. The caller supplies a fallback where it has one.
   */
  branch: string;
  /** Whether HEAD is on no branch at all. */
  detached: boolean;
  /** Uncommitted tracked changes plus untracked files -- both can block. */
  dirtyCount: number;
  /** An operation already under way. Anything but "" blocks before the fetch. */
  inProgress: InProgress;
  /** Whether the checkout was cloned without its history. */
  shallow: boolean;
  /** The remote the update reads. */
  remote: string;
  /** What the remote says the branch is at now, or "" when it could not be read. */
  remoteHead: string;
  /** Why the remote could not be read. Empty when it could. */
  remoteError: string;
  /**
   * Commits this checkout has that the remote does not, and vice versa.
   *
   * `undefined` means NOT KNOWABLE without fetching -- see the header. Callers
   * must render that as unknown rather than as zero.
   */
  ahead?: number;
  behind?: number;
}

/** One git invocation, resolving its stdout. Injected by tests. */
type Run = (args: string[], timeoutMs?: number) => Promise<string>;

/** Whether a path exists. Injected by tests; `fs.existsSync` otherwise. */
type Exists = (path: string) => boolean;

/**
 * The update state, or `undefined` when git cannot answer at all.
 *
 * The two reads that must succeed are the same pair checkoutState.ts requires:
 * `rev-parse HEAD` and `status --porcelain`. A directory where those fail is
 * not a checkout this editor can say anything about. Everything else has a
 * meaningful "no" -- a detached HEAD, an unreachable remote, counts that cannot
 * be computed -- and each is carried rather than thrown.
 */
export async function readUpdateState(
  dir: string,
  opts: { remote?: string; fallbackBranch?: string } = {},
  run: Run = (args, timeoutMs) => git(dir, args, timeoutMs),
  exists: Exists = existsSync,
): Promise<UpdateState | undefined> {
  const remote = (opts.remote ?? "origin").trim() || "origin";
  try {
    const head = (await run(["rev-parse", "HEAD"])).trim();
    const status = await run(["status", "--porcelain"]);
    const current = (await run(["symbolic-ref", "--short", "-q", "HEAD"]).catch(() => "")).trim();
    const gitDir = (await run(["rev-parse", "--absolute-git-dir"]).catch(() => "")).trim();
    const shallow =
      (await run(["rev-parse", "--is-shallow-repository"]).catch(() => "")).trim() === "true";

    const branch = current !== "" ? current : (opts.fallbackBranch ?? "").trim();
    const state: UpdateState = {
      head,
      branch,
      detached: current === "",
      dirtyCount: status.split("\n").filter((l) => l.trim() !== "").length,
      inProgress: inProgressOf(gitDir, exists),
      shallow,
      remote,
      remoteHead: "",
      remoteError: "",
    };
    if (branch === "") return state;

    // ONE remote read, and its failure is a LINE on the checklist rather than a
    // thrown error: an operator on a plane is still entitled to be told what
    // their checkout looks like.
    let remoteHead = "";
    try {
      const refs = await run(
        ["ls-remote", "--heads", remote, `refs/heads/${branch}`],
        UPDATE_REMOTE_TIMEOUT_MS,
      );
      remoteHead = (refs.split("\n")[0] ?? "").split("\t")[0]?.trim() ?? "";
      if (remoteHead === "") {
        state.remoteError = `${remote} has no branch called ${branch}`;
      }
    } catch (err) {
      state.remoteError = err instanceof Error ? err.message.trim() : String(err);
    }
    state.remoteHead = remoteHead;
    if (remoteHead === "" || remoteHead === head) {
      // Equal heads need no object lookup and are the common case: nothing to
      // fetch, so both counts are zero and knowably so.
      if (remoteHead === head) {
        state.ahead = 0;
        state.behind = 0;
      }
      return state;
    }

    // The counts are EXACT or ABSENT. `cat-file -e` is the whole gate: with the
    // commit in the local store `rev-list` answers precisely, and without it
    // there is no honest number to give until the update fetches.
    const known = await run(["cat-file", "-e", `${remoteHead}^{commit}`])
      .then(() => true)
      .catch(() => false);
    if (!known) return state;
    const ahead = await countOrUndefined(run, `${remoteHead}..HEAD`);
    const behind = await countOrUndefined(run, `HEAD..${remoteHead}`);
    if (ahead !== undefined) state.ahead = ahead;
    if (behind !== undefined) state.behind = behind;
    return state;
  } catch {
    return undefined;
  }
}

async function countOrUndefined(run: Run, range: string): Promise<number | undefined> {
  const raw = await run(["rev-list", "--count", range]).catch(() => "");
  const n = Number.parseInt(raw.trim(), 10);
  return Number.isFinite(n) ? n : undefined;
}

/**
 * The unfinished operation a git directory is holding, if any.
 *
 * Asked FIRST of everything that follows it on the checklist, because during
 * one every other read means something else: HEAD is the pre-merge commit and
 * `status --porcelain` reports conflict markers as ordinary modifications.
 *
 * A FILESYSTEM CHECK RATHER THAN A GIT COMMAND, and it is the same one
 * scripts/install/update-stack.sh makes, deliberately -- the checklist and the
 * run have to agree about what "already under way" means or the checklist
 * blesses a run the script then refuses. Three of the four are refs and could
 * have been asked for with `rev-parse`; a rebase leaves only a directory, so
 * one of the four could not, and a mechanism that covered three quarters of the
 * cases would be worse than one that covers all of them the same way.
 */
function inProgressOf(gitDir: string, exists: Exists): InProgress {
  if (gitDir === "") return "";
  const at = (name: string): string => `${gitDir}/${name}`;
  if (exists(at("MERGE_HEAD"))) return "a merge";
  if (exists(at("rebase-merge")) || exists(at("rebase-apply"))) return "a rebase";
  if (exists(at("CHERRY_PICK_HEAD"))) return "a cherry-pick";
  if (exists(at("REVERT_HEAD"))) return "a revert";
  return "";
}

function git(cwd: string, args: string[], timeoutMs = UPDATE_READ_TIMEOUT_MS): Promise<string> {
  return new Promise((resolve, reject) => {
    let child;
    try {
      // `shell: false`, as checkoutState.ts and tags.ts spell it: the directory
      // is an operator's own path and a shell would give it a chance to be more
      // than a path. GIT_TERMINAL_PROMPT=0 so an expired credential fails
      // rather than parking a checklist on an invisible password prompt.
      child = spawn("git", args, {
        cwd,
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
        env: { ...process.env, GIT_TERMINAL_PROMPT: "0" },
      });
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
