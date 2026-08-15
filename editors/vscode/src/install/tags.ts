// The release tags an instance can be moved to.
//
// A wizard-installed local cluster is a detached checkout at a release tag, and
// ArgoCD reconciles the local overlay from it -- so the cluster genuinely has a
// VERSION, and moving it to another tag is a coherent deployment. This lists
// the tags to move to.
//
// IT NEVER AUTO-SELECTS THE NEWEST, and neither does anything downstream.
// `stackPin.ts` argues at length why the pinned tag is a reviewed diff rather
// than whatever was pushed this morning; the same reasoning holds when the
// operator is choosing rather than defaulting. A version somebody picked off a
// list is a fact they can be held to, and one the plugin picked silently is
// not. This module therefore returns an ORDER, never a selection.
//
// AND IT DEGRADES TO TYPING. `git ls-remote` needs a network and a git; an
// operator on a plane has neither and still has a cluster to move. A failure
// here is a listing that came back empty with a reason, which the page renders
// as a free-text field -- not an error that stops the deployment.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3739 #3733

import { spawn, type ChildProcess } from "node:child_process";

/** How long the remote listing is given before the page falls back to typing. */
export const TAG_LIST_TIMEOUT_MS = 10_000;

export interface TagListing {
  /** Semver release tags, newest first. Empty when the listing failed. */
  tags: string[];
  /**
   * Why the listing is empty, when it is. Rendered beside the free-text field
   * so an operator knows they are typing because the network is down rather
   * than because the project has no releases.
   */
  error: string;
}

/**
 * `refs/tags/...` lines from `git ls-remote --tags`, as a newest-first list.
 *
 * THE `^{}` SUFFIX IS DROPPED, NOT SPLIT ON. An annotated tag appears twice --
 * once as the tag object and once as `refs/tags/v1.2.3^{}` for the commit it
 * dereferences to -- and both name the same release. Keeping both would offer
 * the operator the same version twice in a picker.
 *
 * Anything that is not a semver release is dropped: release candidates,
 * nightlies and branch-shaped tags are not versions this install path pins to,
 * and a picker that offered them would invite an operator to move a cluster to
 * something no overlay was reviewed against.
 */
export function parseLsRemoteTags(stdout: string): string[] {
  const seen = new Set<string>();
  for (const line of stdout.split("\n")) {
    const at = line.indexOf("refs/tags/");
    if (at < 0) continue;
    const raw = line.slice(at + "refs/tags/".length).trim();
    const name = raw.endsWith("^{}") ? raw.slice(0, -3) : raw;
    if (parseSemver(name) === undefined) continue;
    seen.add(name);
  }
  return [...seen].sort(compareSemverDesc);
}

interface Semver {
  major: number;
  minor: number;
  patch: number;
}

/**
 * A `vX.Y.Z` release tag, or undefined for anything else.
 *
 * The leading `v` is REQUIRED, because that is the shape this repository's
 * releases take and `DEFAULT_STACK_TAG` is spelled that way. Accepting a bare
 * `1.2.3` would put a tag in the picker that `stackCheckout` would then fail to
 * find, which is a worse outcome than not offering it.
 *
 * A pre-release or build suffix is refused for the reason above: `v1.2.3-rc1`
 * is not a release.
 */
function parseSemver(tag: string): Semver | undefined {
  const m = /^v(\d+)\.(\d+)\.(\d+)$/.exec(tag);
  if (m === null) return undefined;
  return { major: Number(m[1]), minor: Number(m[2]), patch: Number(m[3]) };
}

/**
 * Newest first.
 *
 * Compared NUMERICALLY per component rather than lexicographically, which is
 * the whole reason this is a function: `v0.9.2` sorts after `v0.17.0` as a
 * string, so a lexicographic picker would put a nine-month-old release at the
 * top of the list and call it the newest.
 */
export function compareSemverDesc(a: string, b: string): number {
  const left = parseSemver(a);
  const right = parseSemver(b);
  if (left === undefined || right === undefined) return a.localeCompare(b);
  if (left.major !== right.major) return right.major - left.major;
  if (left.minor !== right.minor) return right.minor - left.minor;
  return right.patch - left.patch;
}

/** Whether a typed value is a tag this install path can check out. */
export function isReleaseTag(value: string): boolean {
  return parseSemver(value.trim()) !== undefined;
}

/** The message for a typed value that is not a release tag. */
export function tagProblem(value: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return "Name the release tag to move this cluster to.";
  if (!isReleaseTag(trimmed)) {
    return `Release tags are spelled vMAJOR.MINOR.PATCH, for example v0.17.0. "${trimmed}" is not one, and the checkout would fail to find it.`;
  }
  return undefined;
}

export interface ListTagsOptions {
  /** The checkout whose origin is asked. */
  cwd: string;
  /**
   * A repository URL to ask INSTEAD of the checkout's origin (memql#3882).
   *
   * The install wizard needs this list BEFORE there is a checkout to ask -- it
   * is choosing what to clone. `git ls-remote --tags <url>` needs no working
   * tree at all, so supplying the URL is what makes the same listing serve both
   * callers: the deployment page asks a cluster's own origin, the wizard asks
   * the repository it is about to clone from.
   */
  repo?: string;
  timeoutMs?: number;
  /** Injectable for tests; the real `git ls-remote` when absent. */
  run?: (cwd: string, timeoutMs: number, repo: string) => Promise<{ stdout: string; error: string }>;
}

/**
 * The release tags the checkout's origin offers, newest first.
 *
 * Never rejects. Every failure -- no git, no network, a timeout, a checkout
 * that is not a repository -- arrives as an empty list with a reason, because
 * the caller's alternative to a list is a text box and not an error dialog.
 */
export async function listReleaseTags(options: ListTagsOptions): Promise<TagListing> {
  const timeoutMs = options.timeoutMs ?? TAG_LIST_TIMEOUT_MS;
  const run = options.run ?? runGitLsRemote;
  try {
    const remote = (options.repo ?? "").trim();
    const result = await run(options.cwd, timeoutMs, remote === "" ? "origin" : remote);
    if (result.error !== "") return { tags: [], error: result.error };
    const tags = parseLsRemoteTags(result.stdout);
    return tags.length === 0
      ? { tags: [], error: "The repository published no release tags." }
      : { tags, error: "" };
  } catch (err) {
    return { tags: [], error: err instanceof Error ? err.message : String(err) };
  }
}

const runGitLsRemote = (
  cwd: string,
  timeoutMs: number,
  remote: string,
): Promise<{ stdout: string; error: string }> =>
  new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let settled = false;
    const finish = (error: string): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        child.kill("SIGKILL");
      } catch {
        // Already gone.
      }
      resolve({ stdout, error });
    };

    let child: ChildProcess;
    try {
      // No shell: the only argument that varies is the cwd, and even that goes
      // through spawn's option rather than into a command line.
      // `remote` is either the literal "origin" or a repository URL, and it is
      // an ARGUMENT rather than part of a command line: `shell: false` means no
      // shell parses it, so a URL cannot smuggle anything through.
      child = spawn("git", ["ls-remote", "--tags", remote], {
        cwd,
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
      });
    } catch (err) {
      resolve({ stdout: "", error: `could not run git: ${(err as Error).message}` });
      return;
    }

    const timer = setTimeout(
      () =>
        finish(
          `git ls-remote did not answer within ${Math.round(timeoutMs / 1000)}s. Type the tag instead.`,
        ),
      timeoutMs,
    );

    child.stdout?.on("data", (chunk: Buffer) => {
      stdout += chunk.toString("utf8");
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString("utf8");
    });
    child.on("error", (err) => finish(`could not run git: ${err.message}`));
    child.on("close", (code) =>
      finish(code === 0 ? "" : (stderr.trim() || `git ls-remote exited ${String(code)}`)),
    );
  });
