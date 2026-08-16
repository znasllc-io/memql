// How a cluster's recorded version relates to the newest release.
//
// Plain Node -- no `vscode` import -- so the decision is unit-testable and the
// surfaces that render it stay thin mappings onto icons and text.
//
// WHY THIS IS NOT install/tags.ts's `parseSemver`
//
// That module feeds the install PICKER. It requires the leading `v` and refuses
// pre-releases outright, and both refusals are load-bearing there: a bare
// `0.18.0` produces a `stackCheckout` that fails to find the ref, and a release
// candidate in the list invites an operator to move a cluster onto something no
// overlay was reviewed against. Widening it to serve this module would put
// release candidates in front of someone installing a cluster.
//
// The question here is a different one. Nothing chose these values from a list:
// they are whatever an install receipt, an ArgoCD targetRevision, a
// deploy-control status, or a live connection happened to report, and the job
// is to place them on the release line or admit that we cannot.
//
// THE FAILURE DIRECTION THAT MATTERS
//
// `notComparable` must never be rendered as "up to date". A plugin talking to a
// cluster older than itself, with nothing anywhere saying so, is the incident
// this epic exists to end. When in doubt this module says it does not know,
// because "I cannot tell" sends an operator to look, and "up to date" does not.

export type VersionRelation = "behind" | "current" | "ahead" | "notComparable";

export interface ParsedRelease {
  readonly major: number;
  readonly minor: number;
  readonly patch: number;
  // The dot-separated pre-release identifiers, empty for a plain release.
  readonly prerelease: readonly string[];
}

// Optional `v`, three components, an optional pre-release, an optional build
// metadata suffix.
//
// Components refuse a leading zero (`(0|[1-9]\d*)` rather than `\d+`) because
// `v01.2.3` is not a tag this repository cuts, and accepting it would make two
// spellings of one release compare equal by accident rather than by rule.
const RELEASE =
  /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/;

// Build metadata is captured and DISCARDED rather than refused. SemVer defines
// it to carry no ordering at all, so `v1.0.0+abc` is unambiguously release
// 1.0.0 -- refusing it would report a value we can place as one we cannot.

const NUMERIC = /^\d+$/;

/**
 * A release, or null for anything else.
 *
 * Tolerates the `v` prefix on either side: `v0.18.0` and `0.18.0` name the same
 * release, and the two sides of a comparison come from sources that do not
 * agree on the prefix (`stackPin.imageTagFor` exists for exactly that skew).
 *
 * REFUSES an all-numeric pre-release, which is the single judgement call in
 * this file. `0.15.0-1737072000` -- the stamp `Dockerfile:178-180` writes over
 * the `VERSION` file -- is a valid pre-release by the letter of SemVer, and
 * honouring that letter would be actively harmful: every currently shipped
 * engine reports this shape, so a cluster genuinely running v0.18.0 would be
 * placed at "0.15.0-something, behind v0.18.0". That is a confident, specific,
 * wrong statement about which release an operator is running. `null` here
 * becomes `notComparable`, which is the truth.
 *
 * One non-digit anywhere in the identifiers is enough to make it a release we
 * cut (`rc1`, `alpha`, `1.rc`), because that is what our pre-release tags look
 * like and a build stamp never does.
 */
export function parseRelease(raw: string | undefined): ParsedRelease | null {
  const m = RELEASE.exec((raw ?? "").trim());
  if (m === null) return null;

  const prerelease = m[4] === undefined ? [] : m[4].split(".");
  // A trailing or doubled dot yields an empty identifier, which SemVer forbids.
  if (prerelease.some((id) => id === "")) return null;
  if (prerelease.length > 0 && prerelease.every((id) => NUMERIC.test(id))) return null;

  return {
    major: Number(m[1]),
    minor: Number(m[2]),
    patch: Number(m[3]),
    prerelease,
  };
}

/**
 * Whether a value names a release at all.
 *
 * This is the RECORD-QUALITY test the version learners use (memql#3993): a
 * value that is not release-shaped must never overwrite one that is, so that a
 * live connection reporting the build stamp cannot replace an install receipt
 * that recorded `v0.18.0`.
 */
export function isReleaseShaped(raw: string | undefined): boolean {
  return parseRelease(raw) !== null;
}

// SemVer pre-release precedence: numeric identifiers compare numerically,
// alphanumeric compare by ASCII order, a numeric identifier ranks BELOW an
// alphanumeric one, and when every shared identifier is equal the longer list
// wins.
function comparePrerelease(a: readonly string[], b: readonly string[]): number {
  // Having a pre-release at all ranks BELOW not having one: v1.0.0-rc.1 comes
  // before v1.0.0. This is the rule the issue names, and it is why an empty
  // list cannot simply be compared as a shorter list.
  if (a.length === 0 && b.length === 0) return 0;
  if (a.length === 0) return 1;
  if (b.length === 0) return -1;

  for (let i = 0; i < Math.min(a.length, b.length); i++) {
    const left = a[i] as string;
    const right = b[i] as string;
    if (left === right) continue;
    const leftNumeric = NUMERIC.test(left);
    const rightNumeric = NUMERIC.test(right);
    if (leftNumeric && rightNumeric) {
      // Numerically, so rc.9 precedes rc.10 -- which string order reverses.
      return Number(left) - Number(right);
    }
    if (leftNumeric !== rightNumeric) return leftNumeric ? -1 : 1;
    return left < right ? -1 : 1;
  }
  return a.length - b.length;
}

function compareParsed(a: ParsedRelease, b: ParsedRelease): number {
  if (a.major !== b.major) return a.major - b.major;
  if (a.minor !== b.minor) return a.minor - b.minor;
  if (a.patch !== b.patch) return a.patch - b.patch;
  return comparePrerelease(a.prerelease, b.prerelease);
}

/**
 * How `recorded` relates to `latest`.
 *
 * `behind` means a newer release exists. `ahead` is an ordinary case rather
 * than an error -- a developer running a locally built cluster -- and must not
 * be reported as behind.
 *
 * Either side being absent, empty, or unparseable yields `notComparable`. Both
 * are expected states, not failures: a cluster may have no recorded version,
 * and the release listing may not have been fetched yet.
 */
export function compareVersions(
  recorded: string | undefined,
  latest: string | undefined,
): VersionRelation {
  const a = parseRelease(recorded);
  const b = parseRelease(latest);
  if (a === null || b === null) return "notComparable";
  const cmp = compareParsed(a, b);
  if (cmp < 0) return "behind";
  if (cmp > 0) return "ahead";
  return "current";
}
