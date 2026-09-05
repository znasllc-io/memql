import { hostnameFor, validateSlug } from "../hostname";
import { normalizeHostname } from "../domains";
import { generateNickname } from "../packages/nickname";
import type { Placement } from "../packages/calls";
import { runCoversApp, shortRepo, type AnalysisReport, type DeploymentRow, type PackageRow } from "../packages/rows";
import type { ZipVerdict } from "../sources/probe";
import type { StopId } from "../targets";

// The compose flow's state, as a function of what has been answered (epic
// memql#4885, task memql#4891).
//
// ===========================================================================
// PURE, AND WHERE THE ASSERTIONS LIVE
// ===========================================================================
// "Deploy is out of reach until every app has a valid address" and "a
// non-GitHub URL cannot reach Analyze" are statements about THIS module, not
// about a picture of it -- the same split `rail.ts` and `list.ts` keep. The
// stops draw its answers.
//
// ===========================================================================
// TWO PATHS, AND WHICH ONE A DRAFT IS ON IS THE ZIP'S ANSWER
// ===========================================================================
// A repository is always the PACKAGE path: the tree is fetched, analyzed and
// parked, and every app the manifest names gets an address. A CI push is
// always the HAND-MADE path: there is nothing to fetch, so the flow makes one
// draft site for somebody's CI to push into. A zip is whichever the probe
// says -- a manifest at the root is a package, an index.html with no manifest
// is a built site, and neither is neither. Nothing here guesses: until
// `artifactProbe` has answered, a zip is on no path at all.

export type SourceChoice = "" | "repo" | "zip" | "ci";

/** Which flow a draft is on, once it is knowable. */
export type ComposePath = "package" | "handmade" | "unknown";

export interface ComposeDraft {
  choice: SourceChoice;
  /** A repository URL as typed. */
  repoUrl: string;
  /** A branch or tag; empty follows the default branch, resolved at fetch time. */
  repoRef: string;
  /** One of the caller's own credentials, or "" to fetch anonymously. */
  credentialId: string;
  /** A Library zip's artifact id. */
  artifactId: string;
  /** What the thing is called: a package's name, a hand-made deployable's title. */
  name: string;
  /** The kind a HAND-MADE deployable takes. A package's kinds come from its manifest. */
  kind: string;
}

export const EMPTY_DRAFT: ComposeDraft = {
  choice: "",
  repoUrl: "",
  repoRef: "",
  credentialId: "",
  artifactId: "",
  name: "",
  kind: "",
};

/** One app's address, as the person is answering it. */
export interface AddressDraft {
  /** The hostname LABEL. The domain half is the cluster's. */
  slug: string;
  /** The client this app is for; "" leaves it untied. */
  accountId: string;
  /** The client's own domain, bound after the site exists. Cluster owners only. */
  ownDomain: string;
  /**
   * Leave this app out of this deploy (memql#4930). A skipped app needs no
   * address, which is the whole point: it is what makes "deploy only the
   * storefront" possible on a FIRST deploy, when neither app has one yet.
   *
   * SKIP IS DEACTIVATE (2026-09-05 design, D5): the name lands on the
   * source's off-list, and the app is listed inactive until somebody
   * activates it from its own row.
   */
  skip?: boolean;
}

// `skip` is ABSENT rather than false: absent means deploy, which is what
// every address written before this field existed means, and what somebody
// who never touches the control means. Writing an explicit false would be a
// value where there was none.
export const EMPTY_ADDRESS: AddressDraft = { slug: "", accountId: "", ownDomain: "" };

/** The one entry a hand-made deployable's address is held under: it has no manifest app name. */
export const HANDMADE_APP = "";

// ---------------------------------------------------------------------------
// The address checks (2026-09-05 design, D7)
// ---------------------------------------------------------------------------

/**
 * What the cluster said about a name while it was being typed.
 *
 * `checking` while the question is out; `ok` when it could be claimed right
 * now; `no` with the server's own sentence otherwise. A check is a courtesy
 * and reserves nothing -- the write guard still decides -- so `ok` means
 * "nothing stood in the way a moment ago", which is exactly enough to stop a
 * person walking the whole flow to learn at the end that the first thing
 * they typed was taken.
 */
export interface AddressVerdict {
  state: "checking" | "ok" | "no";
  /** The reason, in the server's words. Empty unless `no`. */
  problem: string;
}

/** Both halves of one app's address, as checked: the slug, and the client's own domain if any. */
export interface AddressVerdicts {
  slug?: AddressVerdict;
  ownDomain?: AddressVerdict;
}

/** The key one app's checks are held under. The hand-made app has no name, so it takes a fixed one. */
export function addressKeyFor(app: string): string {
  return app === "" ? "one" : app;
}

export function pathOf(draft: ComposeDraft, zip: ZipVerdict | null): ComposePath {
  if (draft.choice === "repo") return "package";
  if (draft.choice === "ci") return "handmade";
  if (draft.choice === "zip") {
    if (zip === "package") return "package";
    if (zip === "built_site") return "handmade";
  }
  return "unknown";
}

/**
 * Whether the SOURCE half is answered, before any address.
 *
 * `probeParked` is the probe's verdict about the repository (sources/probe.ts):
 * a definite answer that this cluster cannot read the tree makes Analyze
 * pointless, because the fetch would refuse with the same information one
 * round trip later. A probe that could not RUN never lands here.
 *
 * `duplicate` is the same posture for a repository this cluster ALREADY
 * TRACKS at that ref (design D8): the engine refuses the second registration,
 * so Analyze is held back and the stop says which source has it.
 */
export function sourceReady(
  draft: ComposeDraft,
  zip: ZipVerdict | null,
  probeParked: boolean,
  duplicate: PackageRow | null = null,
): boolean {
  switch (draft.choice) {
    case "repo":
      return draft.repoUrl.trim() !== "" && draft.name.trim() !== "" && !probeParked && duplicate === null;
    case "zip":
      if (zip === "package") return draft.name.trim() !== "";
      if (zip === "built_site") return draft.name.trim() !== "" && draft.kind.trim() !== "";
      return false;
    case "ci":
      return draft.name.trim() !== "" && draft.kind.trim() !== "";
    default:
      return false;
  }
}

/**
 * Whether one address is answered, as far as a browser can tell.
 *
 * `validateSlug` is the mirrored half of the Go hostname policy and says so
 * (hostname.ts): uniqueness and the cluster-owner exemption are the server's,
 * and their refusals arrive verbatim. An own domain is optional; a blank one
 * binds nothing, and one that normalizes to nothing is a value somebody
 * started typing.
 */
export function addressReady(address: AddressDraft, clusterDomain: string): boolean {
  const slug = address.slug.trim();
  if (slug === "") return false;
  if (validateSlug(slug, clusterDomain) !== "") return false;
  if (address.ownDomain.trim() !== "" && normalizeHostname(address.ownDomain) === "") return false;
  return true;
}

/**
 * Whether one address has CHECKED OUT with the cluster: the slug answered
 * `ok`, and the client's own domain too when one is given. Absent verdicts
 * mean the question has not been asked, which is not an answer.
 */
export function addressChecked(address: AddressDraft, verdicts: AddressVerdicts | undefined): boolean {
  if (verdicts?.slug?.state !== "ok") return false;
  if (address.ownDomain.trim() !== "" && verdicts.ownDomain?.state !== "ok") return false;
  return true;
}

/**
 * Every app the flow must place: the report's, minus the ones that already
 * have a site -- or the one hand-made deployable.
 *
 * A HOSTNAME IS ASKED FOR WHERE IT HAS NEVER BEEN ANSWERED, and nowhere
 * else. The pipeline reads `placements` on a deployable's FIRST deploy only
 * and finds every later one through the (packageId, deployable name) key it
 * recorded, so a field for an app that already serves would ask for a value
 * the engine is going to ignore -- which is the difference between a gate
 * that protects somebody and one they learn to click past.
 *
 * `only` NARROWS THE FORM TO THE ONE APP THE FLOW IS ABOUT (2026-09-05 design,
 * D6). The scope used to reach the wire -- every other app was sent skipped
 * -- and never the form: opened for `web`, Where it lives still asked about
 * `storefront` with a Deploy/Skip pill on each, and the bar counted "2 of 2
 * apps". An app the flow was not opened for is not a question here.
 */
export function appsToPlace(
  path: ComposePath,
  report: AnalysisReport | null,
  placed: readonly string[] = [],
  only = "",
): string[] {
  if (path === "handmade") return [HANDMADE_APP];
  const already = new Set(placed);
  return (report?.deployables ?? [])
    // An app the cluster knows and does not offer gets no site and no
    // address: the engine records it as skipped, and a slug field for one
    // would ask for something nothing can answer at.
    .filter((d) => d.problem === undefined)
    .map((d) => d.name)
    .filter((name) => !already.has(name))
    .filter((name) => only === "" || name === only);
}

/**
 * Whether every app has an address the server could accept -- and, when
 * verdicts are supplied, one the cluster has said is free.
 */
export function placementsComplete(
  apps: readonly string[],
  addresses: Readonly<Record<string, AddressDraft>>,
  clusterDomain: string,
  verdicts?: Readonly<Record<string, AddressVerdicts>>,
): boolean {
  if (apps.length === 0) return false;
  // A SKIPPED APP NEEDS NO ADDRESS, and at least one app has to be going out:
  // a run that would deploy nothing is not a run, and the control says so by
  // being absent rather than by refusing after the click.
  const going = apps.filter((app) => (addresses[app] ?? EMPTY_ADDRESS).skip !== true);
  if (going.length === 0) return false;
  return going.every((app) => {
    const address = addresses[app] ?? EMPTY_ADDRESS;
    if (!addressReady(address, clusterDomain)) return false;
    return verdicts === undefined || addressChecked(address, verdicts[app]);
  });
}

/**
 * The wire form of the addresses: hostnames composed, blank halves OMITTED.
 *
 * An explicit "" would be a value the pipeline reads and acts on -- an empty
 * accountId is a request to tie the site to nothing, an empty ownDomain a
 * request to bind nothing -- so a half nobody answered is absent rather than
 * empty, exactly as `createSite`'s own omitBlank does.
 */
export function placementsFrom(
  apps: readonly string[],
  addresses: Readonly<Record<string, AddressDraft>>,
  clusterDomain: string,
): Record<string, Placement> {
  const out: Record<string, Placement> = {};
  for (const app of apps) {
    const held = addresses[app] ?? EMPTY_ADDRESS;
    // A SKIPPED APP SENDS NO ADDRESS. Nobody was asked where it should live --
    // the field is not even rendered once it is skipped -- so sending the
    // suggestion that was seeded behind the scenes would record a placement
    // the person never saw, let alone chose. Deploying it later is what asks.
    const skipped = held.skip === true;
    out[app] = {
      hostname: skipped ? "" : hostnameFor(held.slug, clusterDomain),
      accountId: held.accountId.trim(),
      ownDomain: normalizeHostname(held.ownDomain),
      ...(held.skip === true ? { skip: true } : {}),
    };
  }
  return out;
}

/**
 * A first name for the source, from the source itself.
 *
 * A STARTING POINT IN AN EDITABLE FIELD, never a decision: somebody who has
 * just pasted a URL should not also have to invent a name, and somebody who
 * wants a different one is one keystroke away.
 */
export function suggestName(draft: ComposeDraft, zipTitle: string): string {
  if (draft.choice === "repo") {
    const parts = shortRepo(draft.repoUrl).split("/").filter((p) => p !== "");
    return parts[parts.length - 1] ?? "";
  }
  if (draft.choice === "zip") return zipTitle.replace(/\.zip$/i, "").trim();
  return "";
}

/**
 * The address a new app starts with: a GENERATED name (2026-09-05 design).
 *
 * It used to be the app's own name -- `storefront`, `web` -- which is what
 * every source declares and therefore what every cluster's first person
 * takes, so the second person found it taken at the end of the flow. The
 * Generate button already drew a memorable two-word name; the seed is the
 * same draw, made before anybody has to press anything. Still a starting
 * point in an editable field: the app's name is one keystroke away for
 * whoever wants it.
 *
 * `random` is injectable for the same reason `generateNickname`'s is.
 */
export function seedAddress(random: () => number = Math.random): AddressDraft {
  return { ...EMPTY_ADDRESS, slug: generateNickname(random) };
}

// ---------------------------------------------------------------------------
// One source, once (2026-09-05 design, D8)
// ---------------------------------------------------------------------------

/**
 * A repository URL reduced to what two registrations are compared on:
 * `host/owner/name`, lowercase, with the scheme, `www.`, a `.git` suffix and
 * a trailing slash removed. Mirrors the engine's `normalizeRepoSource`
 * (component/memql/platform_package_source_policy.go), which is the
 * authority; this is the keystroke-rate half, so the stop can say which
 * source already tracks it before Analyze.
 */
export function normalizeRepoSource(repoUrl: string): string {
  let url = repoUrl.trim().toLowerCase();
  const at = url.indexOf("@");
  if (at >= 0 && !url.slice(0, at).includes("/")) url = url.slice(at + 1).replace(":", "/");
  const scheme = url.indexOf("://");
  if (scheme >= 0) url = url.slice(scheme + 3);
  if (url.startsWith("www.")) url = url.slice(4);
  url = url.replace(/\/+$/, "");
  if (url.endsWith(".git")) url = url.slice(0, -4);
  return url.replace(/\/+$/, "");
}

/**
 * The ACTIVE source that already tracks this repository at this ref, or null.
 *
 * An empty draft ref means the default branch, and `defaultBranch` -- the
 * probe's answer -- is what lets "" and `main` be read as one ref here, which
 * the engine's guard cannot do (it has no network). Archived sources hold
 * nothing: archiving is how a source is added again.
 */
export function duplicateSource(
  packages: readonly PackageRow[],
  repoUrl: string,
  repoRef: string,
  defaultBranch = "",
): PackageRow | null {
  const url = normalizeRepoSource(repoUrl);
  if (url === "") return null;
  const resolve = (ref: string) => {
    const trimmed = ref.trim();
    return trimmed === "" ? defaultBranch.trim() : trimmed;
  };
  const ref = resolve(repoRef);
  return (
    packages.find(
      (p) =>
        p.status !== "archived" &&
        p.sourceKind === "repo" &&
        normalizeRepoSource(p.repoUrl) === url &&
        resolve(p.repoRef) === ref,
    ) ?? null
  );
}

// ---------------------------------------------------------------------------
// The run a flow opened for ONE app is about
// ---------------------------------------------------------------------------

/**
 * Out of a source's timeline (newest first), the run a flow opened for one
 * declared app should read as its own -- or null when none is.
 *
 * `runCoversApp` answers "would this run's work include the app", and a
 * whole-source run answers yes for every app. That is the right reading for
 * a page about an app that HAS a site (memql#4953) and the wrong one here: a
 * source whose whole-source deploy succeeded weeks ago, and which now
 * declares an app that was skipped then or added since, would open that
 * app's flow already FINISHED -- the bar reading Built and offering Done for
 * an app nothing has ever built. It showed exactly that for an inactive app
 * on the source page.
 *
 * So a run that SUCCEEDED counts only when its outcomes record this app at a
 * real address; it deployed something else. A run still moving, parked,
 * refused or failed that covers the app counts as before: it may be the very
 * analysis this flow started, and a single-app source's scoped analysis is
 * unscoped on the wire (there is nothing to skip).
 */
export function runForScopedFlow(runs: readonly DeploymentRow[], app: string): DeploymentRow | null {
  for (const run of runs) {
    if (!runCoversApp(run, app)) continue;
    if (run.status === "succeeded") {
      const placed = run.deployables.some((o) => o.name === app && (o.hostname ?? "").trim() !== "");
      if (!placed) continue;
    }
    return run;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Where the flow IS
// ---------------------------------------------------------------------------

/**
 * The compose flow's phase, from what exists rather than from a held step
 * number.
 *
 * A STEP COUNTER WOULD BE A SECOND SOURCE OF TRUTH, and the one that goes
 * wrong: a run advances by writing its own status on a node nobody in this
 * browser is talking to, so the flow's position is a reading of the rows and
 * never a variable this surface increments. Closing the window and reopening
 * it lands in the same place for the same reason.
 */
export type ComposePhase =
  | "composing"
  | "analyzing"
  | "awaiting_confirm"
  | "deploying"
  | "published"
  | "stopped";

/** The stages a run passes through, in the D6 order the pipeline enforces. */
const DEPLOYING_STATUSES = ["building", "staging_dsl", "rolling", "publishing"];

export function phaseOf(input: {
  path: ComposePath;
  /** The newest run of the source, on the package path. */
  runStatus: string;
  /** The draft site Analyze created, on the hand-made path. */
  siteId: string;
  /** Whether the hand-made publish has landed. A CI-pushed source has nothing to publish from here. */
  published: boolean;
}): ComposePhase {
  if (input.path === "handmade") {
    if (input.siteId === "") return "composing";
    return input.published ? "published" : "awaiting_confirm";
  }
  switch (input.runStatus) {
    case "":
      return "composing";
    case "analyzing":
      return "analyzing";
    case "awaiting_confirm":
      return "awaiting_confirm";
    case "succeeded":
      return "published";
    case "refused":
    case "failed":
      return "stopped";
    default:
      return DEPLOYING_STATUSES.includes(input.runStatus) ? "deploying" : "composing";
  }
}

/** Which stop a run at this status is MOVING at, or null when nothing is moving. */
export function movingStopFor(phase: ComposePhase, runStatus: string): StopId | null {
  if (phase === "analyzing") return "whatItIs";
  if (phase !== "deploying") return null;
  return runStatus === "building" ? "build" : "live";
}
