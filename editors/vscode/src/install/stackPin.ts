// Which memQL release an install checks out when nobody says.
//
// WHY THERE HAS TO BE A DEFAULT AT ALL. `install.cloneStack` requires a tag and
// has no default of its own, deliberately: a checkout that silently follows a
// BRANCH makes "what is installed on this machine" unanswerable a week later,
// so the script refuses a branch outright. That refusal is right. What was
// missing is the other half -- somewhere for the answer to come from when the
// operator has not typed one.
//
// The CLI takes `--tag`, so `cli.js install --tag=v1.4.0` always worked. The
// wizard has no tag field and passed none, so `installPlan` dropped the empty
// value and every install started from the "+" button died at stackCheckout
// with `exit 2: missing required parameter: tag` -- an exit code whose guidance
// correctly reads "a fault in memQL rather than in your machine", and it was
// (memql#3560).
//
// WHY A PINNED CONSTANT RATHER THAN "THE NEWEST TAG". Resolving the latest
// release at run time would need no maintenance, and it is the wrong trade for
// this repository. `scripts/install/tool-pins.env` already states the house
// position for k3d, kubectl and mkcert: "Changing a pin is a reviewed diff,
// never a silent auto-update." The same reasoning applies with more force here,
// because a packaged extension carries a STAGED COPY of `scripts/` from its own
// build commit and runs those scripts against whatever the checkout contains. A
// pin makes that pairing a fact somebody chose and can read off a diff; "newest
// tag" makes it whatever was pushed this morning.
//
// KEEPING IT CURRENT is therefore a release step, exactly like bumping a tool
// pin: cut the tag, then bump this. `installSession.test.ts` checks the SHAPE,
// not the recency -- no test can tell a deliberate pin from a stale one, and
// one that reached the network to try would fail offline for a reason that has
// nothing to do with the change under test.
//
// WHY IT IS NOT v0.15.0, WHICH WAS THE NEWEST TAG WHEN THIS LANDED. The install
// path is younger than the last release. v0.15.0's local overlay carries no
// `bootstrap-secret-envfrom` patch (#3375), so `seedBootstrap` would write the
// identity bootstrap secret and no pod would read it -- the install would run
// to the end and leave a cluster with no owner and no way in. A pin is only as
// good as the release it names, which is why bumping it belongs to the release
// and not to whoever last touched the installer.
//
// Refs: #3560 #3375 #3363 #3357

/**
 * The release tag an install checks out unless told otherwise.
 *
 * Overridden per run by `SessionOptions.tag` (`--tag` on the CLI). Applied in
 * `installPlan`, so no front end can forget it.
 */
export const DEFAULT_STACK_TAG = "v0.16.0";

/**
 * How a cluster the wizard builds treats people who are not its owner.
 *
 * `invite_only`, because that cluster has exactly one owner -- bootstrapped
 * from the answers on the same screen -- and lives at a hostname on the
 * operator's own machine. `open` would let anyone who can reach it register an
 * account. Overridden per run by `--registration-mode` on the CLI.
 *
 * It lives beside the stack pin because it is the same kind of value: a default
 * the installer supplies so that neither front end has to remember to, applied
 * in `installPlan` where both go through (memql#3568, memql#3560).
 */
export const DEFAULT_REGISTRATION_MODE = "invite_only";

/**
 * Where an install pulls the node images from.
 *
 * WHY AN INSTALL CANNOT USE THE OVERLAY'S OWN. The local overlay renames every
 * node image to `memql-<node>:local` -- images that exist only after a
 * developer runs `make dev` to build and import them. That is right for the
 * inner loop and impossible for an install, so every pod sat in
 * ImagePullBackOff (memql#3572).
 *
 * GHCR rather than the ACR the base manifests name, because ACR is private and
 * reachable only by the deployment that owns it. Somebody installing a local
 * cluster has no Azure credentials and no reason to acquire any. The images are
 * identical -- one build, pushed to both.
 *
 * The tag is DEFAULT_STACK_TAG: the manifests and the images they run come from
 * the same release, which is the whole point of pinning either.
 */
export const DEFAULT_IMAGE_REGISTRY = "ghcr.io/znasllc-io";

/**
 * The IMAGE tag for a release tag.
 *
 * TWO CONVENTIONS, ONE RELEASE. Git tags carry the `v` (`v0.16.0`, and
 * clone-stack.sh checks out exactly that); image tags do not -- the base
 * manifests name `acrmemql.azurecr.io/memql-identity:0.9.9`, and
 * build-engine-images.yml takes its `version` input as the tag verbatim,
 * documented as "e.g. 0.9.61".
 *
 * Both are long-standing and neither is wrong, so the installer converts rather
 * than asking anyone to remember which surface wants which. Passing the git tag
 * straight through would ask a registry for `memql-bff:v0.16.0`, which is not a
 * tag anything publishes -- an ImagePullBackOff whose cause is one character
 * (memql#3574).
 */
export function imageTagFor(releaseTag: string): string {
  return releaseTag.replace(/^v/, "");
}
