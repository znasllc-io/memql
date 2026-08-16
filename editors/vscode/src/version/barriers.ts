// Releases you cannot simply retag across.
//
// AN UPGRADE IS NORMALLY THE INSTALL GRAPH RE-RUN WITH A DIFFERENT TAG --
// `stackCheckout` moves the checkout, `clusterUp` reconciles the local overlay,
// every other step verifies and skips (state/upgradePlan.ts says this at
// length). That property is what makes one button honest.
//
// It is not true across every pair of releases. Moving past v0.18.0 changes
// what the local cluster's database IS: that overlay lists `postgres.yaml`, a
// plain in-overlay Deployment, and main's composes `components/cnpg-db` -- a
// CloudNativePG cluster -- while `scripts/k3d/up.sh` gained
// `install_operator_stack`, which registers cert-manager and CloudNativePG
// before anything reconciles. Moving the checkout does not migrate the data in
// the old Deployment's volume, and nothing downstream notices: `clusterUp`
// reconciles cleanly onto a fresh CNPG cluster and the operator gets a working
// cluster with an empty graph.
//
// So the table below, and a refusal rather than a confirmation when a move
// crosses one.
//
// WHY A HAND-MAINTAINED LIST AND NOT A DERIVATION
//
// Same reason `TAG_SENSITIVE_STEPS` is a list (state/upgradePlan.ts): the
// derivation would have to be "which releases changed something the checkout
// move cannot carry", and that is a fact about what shipped in each release
// rather than about anything the plugin can read. A list a reviewer can check
// against the release beats an inference that looks principled and is not --
// and the failure mode of a wrong inference here is silent data loss, which is
// the worst thing in this epic's blast radius.
//
// The list is a REVIEWED artifact, in the same sense DEFAULT_STACK_TAG is: an
// entry is added by the person who shipped the barrier, in the release that
// shipped it. `barriers.test.ts` holds the newest entry at or below
// DEFAULT_STACK_TAG so the table cannot describe a release the installer
// cannot reach.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3999 #3989

import { compareVersions, isReleaseShaped } from "./compare.js";

export interface UpgradeBarrier {
  /**
   * The last release BEFORE the barrier -- the barrier sits in the gap
   * immediately after it.
   *
   * So `v0.18.0` means: v0.17.1 -> v0.18.0 is an ordinary retag, and
   * v0.18.0 -> anything newer is not. Naming the release the barrier FOLLOWS
   * rather than the one that introduced it means an entry can be written the
   * moment the change lands, before the release carrying it has a number.
   */
  readonly afterVersion: string;
  /** What the move actually changes, in one sentence an operator can act on. */
  readonly summary: string;
  /**
   * The runbook, as a REPO-RELATIVE path.
   *
   * Not a URL, and the choice is deliberate: this repository publishes no docs
   * site, so a URL would be an unverifiable string in a table whose entire
   * purpose is being checkable. A path can be asserted to exist, and
   * `barriers.test.ts` does exactly that -- a refusal that points at a
   * document nobody wrote is worse than no refusal, because it reads as
   * though someone thought about it.
   */
  readonly docHref: string;
}

/**
 * Every barrier, oldest first.
 *
 * ORDER IS PART OF THE DATA, not a convenience: a move can cross more than one
 * barrier, and an operator crossing two needs them in the order they will be
 * performed. `barriers.test.ts` holds the ordering rather than trusting it.
 */
export const UPGRADE_BARRIERS: readonly UpgradeBarrier[] = [
  {
    afterVersion: "v0.18.0",
    summary:
      "The local cluster's database changes from an in-overlay Postgres Deployment to a " +
      "CloudNativePG cluster, and the cluster gains an operator stack (cert-manager + " +
      "CloudNativePG) that must be registered before the overlay reconciles. Moving the " +
      "checkout does not carry the data in the old Deployment's volume across.",
    docHref: "docs/public/operate/upgrade-barriers.md",
  },
];

/**
 * Whether `version` sits at or before `barrier` on the release line.
 *
 * `undefined` means "cannot tell" and is NOT folded into either answer, which
 * is the whole reason this returns a nullable boolean rather than a boolean.
 */
function atOrBefore(version: string | undefined, barrier: string): boolean | undefined {
  const relation = compareVersions(version, barrier);
  if (relation === "notComparable") return undefined;
  return relation === "behind" || relation === "current";
}

export type BarrierCheck =
  /** Both versions place on the release line, and no barrier lies between them. */
  | { readonly kind: "clear" }
  /** Both versions place, and these barriers lie between them, in crossing order. */
  | {
      readonly kind: "blocked";
      readonly barriers: readonly UpgradeBarrier[];
      /** `backward` when the move goes to an OLDER release. */
      readonly direction: "forward" | "backward";
    }
  /**
   * At least one side does not place on the release line, so whether a barrier
   * lies between them is unknown.
   */
  | {
      readonly kind: "undetermined";
      readonly unreadable: "from" | "to" | "both";
      readonly from: string | undefined;
      readonly to: string | undefined;
    };

/**
 * Which barriers a move from `from` to `to` crosses.
 *
 * SYMMETRIC IN DIRECTION. A downgrade across a barrier is a crossing too, and
 * if anything a worse one -- moving back before v0.18.0 puts a plain Postgres
 * Deployment in front of data that lives in a CloudNativePG cluster. The
 * instance action is deliberately called "Move this cluster to another release
 * tag" rather than "upgrade" for exactly this reason
 * (deploy/instanceActions.ts), so a check that only looked forwards would be
 * blind to half of what that button can do.
 *
 * A barrier is crossed when exactly ONE of the two versions is at or before it.
 * That reading makes the interval half-open at the right end, which is what
 * `afterVersion` means: v0.17.1 -> v0.18.0 crosses nothing, v0.18.0 -> newer
 * crosses the v0.18.0 barrier.
 *
 * `undetermined` IS NOT `clear`, AND THIS IS THE POINT.
 *
 * The fleet these barriers protect is largely the fleet whose version does not
 * place: every engine shipped before memql#3998 reports the build stamp
 * `0.15.0-<epoch>`, which compare.ts deliberately refuses to read as a release,
 * and a cluster installed before memql#3990 has no recorded version at all. If
 * an unreadable version quietly returned `clear`, the barrier table would be
 * inert for precisely the clusters it exists to protect, while looking like it
 * worked.
 *
 * It is not this module's job to decide what `undetermined` costs the operator
 * -- refusing every move with an unknown version would make the button useless
 * on day one, and proceeding silently is the failure above. The caller renders
 * the three states; this reports which one is true.
 */
export function barriersCrossed(
  from: string | undefined,
  to: string | undefined,
): BarrierCheck {
  const fromPlaces = isReleaseShaped(from);
  const toPlaces = isReleaseShaped(to);
  if (!fromPlaces || !toPlaces) {
    return {
      kind: "undetermined",
      unreadable: !fromPlaces && !toPlaces ? "both" : !fromPlaces ? "from" : "to",
      from,
      to,
    };
  }

  const crossed = UPGRADE_BARRIERS.filter((barrier) => {
    const before = atOrBefore(from, barrier.afterVersion);
    const after = atOrBefore(to, barrier.afterVersion);
    // Both sides place, so neither can be undefined here -- but the check is
    // written as a comparison rather than an assertion so a future barrier
    // whose own afterVersion stopped parsing degrades to "not crossed by
    // anything" instead of throwing mid-confirmation. The test that every
    // afterVersion is release-shaped is what actually prevents that.
    return before !== undefined && after !== undefined && before !== after;
  });

  if (crossed.length === 0) return { kind: "clear" };

  const backward = compareVersions(to, from) === "behind";
  return {
    kind: "blocked",
    // Oldest-first in the table; a backward move performs them in reverse.
    barriers: backward ? [...crossed].reverse() : crossed,
    direction: backward ? "backward" : "forward",
  };
}
