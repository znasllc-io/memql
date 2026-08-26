// Which MemQL release an install checks out when nobody says.
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
// correctly reads "a fault in MemQL rather than in your machine", and it was
// (memql#3560).
//
// WHAT THIS PIN IS FOR NOW: THE OFFLINE FALLBACK (memql#4429). It is the
// release the extension was built against, and it is what an install uses when
// it cannot ask the repository what releases exist -- no network, no git, a
// listing that timed out. It is no longer what a connected install gets: the
// version field is seeded from the listing and the picker labels that entry
// `Latest -- vX.Y.Z (recommended)`.
//
// THE ARGUMENT THAT USED TO STAND HERE, AND WHY IT NO LONGER DOES. This section
// argued for a pinned constant OVER "the newest tag", by analogy with
// `scripts/install/tool-pins.env` ("Changing a pin is a reviewed diff, never a
// silent auto-update"), on the grounds that a packaged extension carries a
// STAGED COPY of `scripts/` from its own build commit and runs those scripts
// against whatever the checkout contains -- so the pairing should be something
// somebody chose.
//
// The analogy is the part that failed. A tool pin names a THIRD-PARTY tool
// whose new releases nobody here reviewed; this names OUR OWN release, cut from
// this repository, and every postmortem recorded below is a failure of
// installing an OLDER one. Read them in order: v0.16.1 issued magic links at a
// host nobody could reach; v0.17.1 routed nothing at api.<domain>; v0.18.0
// severed sessions on an unknown field; v0.19.0 shipped an inert recovery key.
// Not one of them is a failure of having installed something too new. A pin
// that must be bumped by hand is a pin that is stale by default, and "the
// reviewed pairing" was in practice "whatever the last person to touch this file
// happened to have".
//
// WHAT SURVIVES OF IT is the offline case, where there is no listing to prefer
// and the reviewed pairing is genuinely the best available answer, and the
// release-step discipline below -- the pin is still bumped at a release, because
// it is still what an offline machine installs.
//
// KEEPING IT CURRENT is therefore still a release step, exactly like bumping a
// tool pin: cut the tag, then bump this. `installSession.test.ts` checks the
// SHAPE, not the recency -- no test can tell a deliberate pin from a stale one,
// and one that reached the network to try would fail offline for a reason that
// has nothing to do with the change under test.
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
// identity.example.com -- a host nobody can reach, so no owner account is
// ever created and the install ends green with no way in. A pin is only as good
// as the release it names, which is why bumping it belongs to the release and
// not to whoever last touched the installer.
//
// BUMPED TO v0.18.0 (memql#3879, memql#3880, memql#3703). A clean uninstall
// followed by a clean install produced a cluster that could not be used, and
// every part of that sequence was working correctly. The install faithfully
// deployed v0.17.1; all three fixes were on main.
//
// The one worth reading twice is the front door, because it shows what a stale
// pin actually costs. v0.17.1's local overlay carries `front-door.yaml`,
// `cockpit-front-door.yaml` and `identity-external.yaml` -- and no
// `api-front-door.yaml`, which arrived with the five-host front door. So NOTHING
// ROUTED `api.memql.localhost`, the one host every client dials. The editor
// extension reported `websocket open failed: Unexpected server response: 404`
// and the operator reasonably read it as a extension fault. It was a manifest that
// had never been cut into a release.
//
// The other two were quieter and pointed at the wrong component just as hard.
// memql#3879: a v0.17.1 engine that loses the race with Postgres never retries
// its migrations, so the database stays EMPTY and identity answers `500 could
// not persist client` -- a failure that names the identity service for a fault
// in database startup. memql#3880: seedBootstrap spent one shared roll budget
// serially across seven consumers, so the install failed on whichever node the
// clock happened to run out on and blamed that node by name.
//
// WHAT THIS ADDS TO THE ARGUMENT ABOVE. The v0.16.1 note already says a pin is
// only as good as the release it names. What v0.17.1 adds is that a stale pin
// does not fail like a stale pin. Each symptom accused a healthy component --
// the extension, identity, cognition -- and none of them pointed at the version.
// Bumping this is the release step; skipping it spends somebody's afternoon.
//
// BUMPED TO v0.19.0 (memql#4059). 262 commits accumulated behind v0.18.0, and
// the two failures that surfaced are the two this comment keeps recording: a
// stale pin does not fail like a stale pin, and neither symptom named the
// version.
//
// The first was not the pin's fault, and is worth stating because it is the
// mirror image of the pin problem. `install.cloneStack` reported DONE, with the
// correct commit on its envelope, over a directory holding a .git and no files
// -- an earlier install had been interrupted mid-clone, and every check the
// script made was a REF check. The install then failed at `clusterUp` with
// "it is a git checkout, but not of MemQL", which is a true sentence about the
// wrong problem: it was MemQL, with nothing checked out of it. Fixed in
// scripts/install/clone-stack.sh, and the reason it belongs in this release is
// that the repair only reaches an operator through a release.
//
// The second is the skew this pin exists to prevent, and v0.18.0 is the release
// that demonstrates it best. `AuthoringValidateBundleMsg.origin` landed SIX
// HOURS after the v0.18.0 tag was cut, so no release carries it; a extension built
// past the tag sends it, the cluster refuses it, and because the WebSocket
// bridge decodes with unknown fields on, the refusal severs the session instead
// of failing one request. The operator sees `ERROR (validate): stream closed`
// and nothing says their cluster is simply older than their extension. See
// version/skewHint.ts, which exists only because the pin was allowed to go
// stale.
//
// This bump also carries the cluster across the v0.18.0 upgrade barrier
// (version/barriers.ts): the database becomes a CloudNativePG cluster, so a
// machine already installed at v0.18.0 does NOT move here without the runbook.
// A fresh install has nothing to carry and crosses it for free.
//
// v0.19.1 carries the OWNER RECOVERY KEY, which was inert on every cluster
// v0.19.0 installed (#4066). Both halves of the break-glass credential were
// refused by the engine: the @serverOnly reads because nothing stamped internal
// origin, and every credential write because it used the system SERVICE actor
// rather than the system CREDENTIAL actor the memql#2513 guard admits. So a
// v0.19.0 install reaches its last step, `recoveryKey`, and fails there -- and
// even if that step were skipped, the cluster would be left with no break-glass
// route for its owner, announced only in a WARN nothing surfaces.
//
// That makes this bump load-bearing rather than housekeeping: the fix is ENGINE
// code, so it arrives in the node images, and the images an install pulls are
// the ones this tag names. Leaving the pin at v0.19.0 would leave every new
// install with the broken credential no matter what main contains.
//
// Refs: #4066 #4063 #4059 #3998 #3990 #3879 #3880 #3703 #3602 #3600 #3593 #3560 #3375 #3363 #3357

/**
 * The release tag an install checks out when nothing else names one.
 *
 * THE OFFLINE FALLBACK the extension was built against (memql#4429), not the
 * house recommendation: a connected install is seeded with the newest release
 * the repository lists and the picker labels it `Latest ... (recommended)`.
 * This is what remains when that listing cannot be had.
 *
 * Overridden per run by `SessionOptions.tag` (`--tag` on the CLI). Applied in
 * `installPlan`, so no front end can forget it.
 */
export const DEFAULT_STACK_TAG = "v0.19.1";

/**
 * The value the version picker carries for "the tip of main" (memql#3901).
 *
 * A SENTINEL RATHER THAN A TAG-SHAPED STRING, because every consumer has to be
 * able to tell it apart: `isReleaseTag` must reject it, `imageTagFor` must not
 * be handed it, and `installPlan` has to send `--branch` instead of `--tag`. A
 * value that looked like a tag would be silently accepted by all three and fail
 * at the far end as an ImagePullBackOff.
 */
export const MAIN_BRANCH_CHOICE = "main";

/**
 * Whether a chosen version means "the tip of main" rather than a release.
 *
 * Exported so the branch/tag decision is made in ONE place. `installPlan`, the
 * image-tag derivation and the picker all ask this rather than each comparing
 * against a literal.
 */
export function isMainBranchChoice(version: string): boolean {
  return version.trim() === MAIN_BRANCH_CHOICE;
}

/**
 * The registry image tag for a chosen RELEASE, and it refuses `main`.
 *
 * THE MAIN LANE HAS NO REGISTRY IMAGE, BY CONSTRUCTION (memql#4430). `main`
 * BUILDS its node images from the checkout it just cloned -- the `buildImages`
 * step, the same `k3d.dev` capability "Rebuild from checkout" drives -- and
 * imports them into k3d as `memql-<node>:local`. There is nothing for this
 * function to answer on that lane, so `installPlan` omits `--image-registry`
 * and `--image-tag` entirely and the overlay's own `:local` images stand.
 *
 * IT THROWS RATHER THAN GUESSING, and the throw is the point. This used to map
 * `main` to the newest RELEASE's images, which made a `main` install a
 * deliberate skew: main's manifests and scripts, an older release's engine
 * binary. That skew is gone. What must not replace it is a silent fallback --
 * an `imageTagFor("main")` asking a registry for `memql-bff:main`, a tag
 * nothing publishes, is an ImagePullBackOff on every pod whose cause is one
 * word. `isMainBranchChoice` stays the ONE discriminator; every caller asks it
 * first, and this refusal is what makes a caller that forgot fail at the call
 * rather than forty pods later.
 */
export function imageTagForVersion(version: string): string {
  if (isMainBranchChoice(version)) {
    throw new Error(
      "imageTagForVersion was handed `main`, which names no published image: a main install BUILDS its node images from the checkout (see the buildImages step). Ask isMainBranchChoice first and omit the registry flags on that lane.",
    );
  }
  return imageTagFor(version.trim() || DEFAULT_STACK_TAG);
}

/**
 * The repository an install clones, and the one the version list is read from.
 *
 * It lives here rather than in the panel for the reason every other default in
 * this file does: `clone-stack.sh` already carries it as its own `DEFAULT_REPO`,
 * and a second literal is how the two drift. The wizard needs it BEFORE a
 * checkout exists -- it is choosing what to clone -- so `git ls-remote --tags`
 * is pointed at this URL rather than at an origin there is not yet.
 */
export const DEFAULT_STACK_REPO = "https://github.com/znasllc-io/memql.git";

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
 * `~/.memql/mkcert` is MemQL's own directory, identical whether the installer
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
 * 6761. Its WebAuthn RP id is accepted -- measured by the memql#3405 spike,
 * where bare `localhost` is refused as a public suffix and `memql.localhost` is
 * not -- so one passkey covers every host under it.
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
/**
 * Why a local install cannot be driven from a remote window (memql#4623).
 *
 * `undefined` locally; a sentence otherwise.
 *
 * THE INSTALL IS NOT AN ADDRESS, IT IS A MACHINE. It writes hosts entries,
 * issues an mkcert certificate into a trust store, creates a k3d cluster and
 * bootstraps an owner -- all on the EXTENSION HOST, which under Remote-SSH, a
 * dev container, WSL or Codespaces is the far end of a connection. The browser
 * is on the near end. Three things then fail together and none of them says so:
 *
 *   - every credential link the wizard hands over is
 *     `https://identity.memql.localhost/...`, and RFC 6761 makes the near-end
 *     browser resolve the whole `.localhost` family to its OWN loopback, where
 *     no cluster is running;
 *   - `asExternalUri` cannot help, because it tunnels loopback AUTHORITIES and
 *     `identity.memql.localhost` is a name, not one;
 *   - the mkcert CA went into the FAR end's trust store, so even a forwarded
 *     port would fail the certificate.
 *
 * The install would SUCCEED and every button on the done screen would open a
 * tab that cannot connect -- which is the worst available outcome, because the
 * cluster is real and the operator has no way to reach it. Refusing costs them
 * one window.
 */
export function localInstallRemoteProblem(remoteName: string | undefined): string | undefined {
  const remote = (remoteName ?? "").trim();
  if (remote === "") return undefined;
  return (
    `A local cluster cannot be installed from a ${remote} window. Everything the install ` +
    `writes -- hosts entries, the mkcert certificate, the cluster itself -- lands on the ` +
    `remote host, while your browser is on this one, so the cluster would come up and none ` +
    `of its sign-in links would open. Open a local window to install, then register the ` +
    `cluster from anywhere with "Connect to an existing cluster".`
  );
}

export function installDomainProblem(domain: string): string | undefined {
  const trimmed = domain.trim();
  if (trimmed === "") return undefined;
  if (DOMAIN_PATTERN.test(trimmed)) return undefined;

  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed)) {
    return "Enter a domain, not a URL: MemQL adds the scheme and the front-door hostnames itself, so `memql.localhost` rather than `https://memql.localhost`.";
  }
  if (trimmed.includes(":")) {
    return "Enter a domain with no port. The front door is on 443 and MemQL puts the cluster there itself.";
  }
  if (trimmed.startsWith("*.")) {
    return "Enter the domain itself, not a wildcard. MemQL derives `api.` and `identity.` from it, and the certificate covers the wildcard for you.";
  }
  if (!trimmed.includes(".")) {
    return `Enter a domain with at least two labels, such as ${DEFAULT_LOCAL_DOMAIN}. A single label cannot carry the front-door subdomains MemQL needs.`;
  }
  return `That is not a domain MemQL can serve. Use lowercase letters, digits and hyphens, with at least two labels -- for example ${DEFAULT_LOCAL_DOMAIN}.`;
}
