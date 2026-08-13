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
// BUMPED TO v0.17.0 (memql#3600, memql#3602). Two bugs made a v0.16.1 install
// produce a cluster nobody could sign into, and only one of them was in the
// installer.
//
// memql#3602: clone-stack.sh leaves a DETACHED checkout at this tag, and
// `git rev-parse --abbrev-ref HEAD` answers the literal "HEAD" for one, which
// ArgoCD resolves to the default branch. So every install so far pinned its
// IMAGES to this tag and reconciled its MANIFESTS from main. The two drifted the
// moment they disagreed: main's overlay stopped setting identity's domain env
// because the engine derives it now (memql#3593), and a v0.16.1 binary derives
// nothing.
//
// That is also why this pin cannot stay at v0.16.1 now that the skew is fixed.
// With the manifests finally following the pin, a v0.16.1 install would
// faithfully serve the PREVIOUS default domain while this wizard writes hosts
// entries and a
// certificate for memql.localhost -- correctly pinned, and still wrong. The pin
// and the default domain have to move together, and v0.17.0 is the first release
// carrying both.
//
// WHY IT IS NOT v0.16.1, WHICH WAS THE NEWEST TAG WHEN THIS LANDED. The install
// path is younger than the last release, and that keeps being true: v0.16.1's
// local overlay pins identity's domain env to staging placeholders that defeat
// the derivation (memql#3600), so identity would issue a magic link at
// identity.staging.example.com -- a host nobody can reach, so no owner account is
// ever created and the install ends green with no way in. A pin is only as good
// as the release it names, which is why bumping it belongs to the release and
// not to whoever last touched the installer.
//
// Refs: #3602 #3600 #3593 #3560 #3375 #3363 #3357

/**
 * The release tag an install checks out unless told otherwise.
 *
 * Overridden per run by `SessionOptions.tag` (`--tag` on the CLI). Applied in
 * `installPlan`, so no front end can forget it.
 */
export const DEFAULT_STACK_TAG = "v0.17.1";

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

/**
 * Where the local certificate authority lives.
 *
 * WHY IT IS PINNED AT ALL. mkcert derives CAROOT from `XDG_DATA_HOME`, and
 * snapd sets that to a REVISION-SCOPED directory for a snap-packaged editor:
 *
 *     XDG_DATA_HOME=/home/you/snap/code/255/.local/share
 *
 * So an install run from VS Code put the CA in `.../code/255/...` while
 * `mkcert -CAROOT` in the operator's own terminal answered
 * `~/.local/share/mkcert`. Two consequences, both observed (memql#3576): the
 * installer and the operator disagreed about which CA was "the" CA and a second
 * one got created; and `255` is baked into the path the receipt records, so the
 * next snap refresh strands it and every refresh accumulates another.
 *
 * `~/.memql/mkcert` is memQL's own directory, identical whether the installer
 * or a human runs mkcert, and independent of how the editor was packaged.
 */
export const DEFAULT_CAROOT_DIR = ".memql/mkcert";

/**
 * The domain a local install serves unless the operator says otherwise.
 *
 * A DEFAULT NOW, NOT A CONSTRAINT (memql#3593). This used to be the one value
 * the installer accepted, because the release's local overlay pinned its
 * Ingress hosts and identity configuration and nothing the installer passed
 * could change them. That overlay is parameterised now: the domain reaches the
 * cluster as the single key of a `memql-domain` ConfigMap, from which every
 * domain-shaped value is derived at boot, and -- when it differs from the
 * overlay's committed default -- as two patches on the ArgoCD Application.
 *
 * `memql.localhost` rather than a company's domain, because the engine is meant
 * to carry no product and a hostname is product. It needs no domain ownership,
 * no DNS provider and no third party: `.localhost` resolves to loopback by RFC
 * 6761. Its WebAuthn RP id is accepted -- measured in
 * scripts/spikes/webauthn-rpid (memql#3405), where bare `localhost` is refused
 * as a public suffix and `memql.localhost` is not -- so one passkey covers every
 * host under it.
 *
 * Kept in step with deploy/k8s/overlays/local, whose two Ingresses carry this
 * same apex as their committed default. installDomain.test.ts asserts that
 * agreement against the shipped files.
 */
export const DEFAULT_LOCAL_DOMAIN = "memql.localhost";

/** What a domain may look like: two or more lowercase labels, nothing else. */
const DOMAIN_PATTERN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/;

/**
 * Why this domain will not work, or nothing.
 *
 * SYNTAX ONLY. Whether a domain RESOLVES is not knowable here and is not this
 * function's job: `hostsBlock` probes it and either writes the entry or refuses
 * with the address it actually answered, and `frontDoor` checks the whole path
 * end to end. What this catches is the answer that cannot become a hostname at
 * all -- a pasted URL, a port, a wildcard -- before ten minutes of work proves
 * it.
 *
 * Empty is accepted: an empty field is "not answered yet", which the required-
 * field check reports in its own words rather than as a domain that is wrong.
 */
export function installDomainProblem(domain: string): string | undefined {
  const trimmed = domain.trim();
  if (trimmed === "") return undefined;
  if (DOMAIN_PATTERN.test(trimmed)) return undefined;

  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed)) {
    return "Enter a domain, not a URL: memQL adds the scheme and the front-door hostnames itself, so `memql.localhost` rather than `https://memql.localhost`.";
  }
  if (trimmed.includes(":")) {
    return "Enter a domain with no port. The front door is on 443 and memQL puts the cluster there itself.";
  }
  if (trimmed.startsWith("*.")) {
    return "Enter the domain itself, not a wildcard. memQL derives `cockpit.` and `identity.` from it, and the certificate covers the wildcard for you.";
  }
  if (!trimmed.includes(".")) {
    return `Enter a domain with at least two labels, such as ${DEFAULT_LOCAL_DOMAIN}. A single label cannot carry the front-door subdomains memQL needs.`;
  }
  return `That is not a domain memQL can serve. Use lowercase letters, digits and hyphens, with at least two labels -- for example ${DEFAULT_LOCAL_DOMAIN}.`;
}
