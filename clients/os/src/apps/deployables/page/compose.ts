import { hostnameFor, validateSlug } from "../hostname";
import { normalizeHostname } from "../domains";
import { suggestSlug } from "../packages/hostname";
import type { Placement } from "../packages/calls";
import { shortRepo, type AnalysisReport } from "../packages/rows";
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
 */
export function sourceReady(draft: ComposeDraft, zip: ZipVerdict | null, probeParked: boolean): boolean {
  switch (draft.choice) {
    case "repo":
      return draft.repoUrl.trim() !== "" && draft.name.trim() !== "" && !probeParked;
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
 * Every app the flow must place: the report's, minus the ones that already
 * have a site -- or the one hand-made deployable.
 *
 * A HOSTNAME IS ASKED FOR WHERE IT HAS NEVER BEEN ANSWERED, and nowhere
 * else. The pipeline reads `placements` on a deployable's FIRST deploy only
 * and finds every later one through the (packageId, deployable name) key it
 * recorded, so a field for an app that already serves would ask for a value
 * the engine is going to ignore -- which is the difference between a gate
 * that protects somebody and one they learn to click past.
 */
export function appsToPlace(
  path: ComposePath,
  report: AnalysisReport | null,
  placed: readonly string[] = [],
): string[] {
  if (path === "handmade") return [HANDMADE_APP];
  const already = new Set(placed);
  return (report?.deployables ?? [])
    // An app the cluster knows and does not offer gets no site and no
    // address: the engine records it as skipped, and a slug field for one
    // would ask for something nothing can answer at.
    .filter((d) => d.problem === undefined)
    .map((d) => d.name)
    .filter((name) => !already.has(name));
}

/** Whether every app has an address the server could accept. */
export function placementsComplete(
  apps: readonly string[],
  addresses: Readonly<Record<string, AddressDraft>>,
  clusterDomain: string,
): boolean {
  if (apps.length === 0) return false;
  // A SKIPPED APP NEEDS NO ADDRESS, and at least one app has to be going out:
  // a run that would deploy nothing is not a run, and the control says so by
  // being absent rather than by refusing after the click.
  const going = apps.filter((app) => (addresses[app] ?? EMPTY_ADDRESS).skip !== true);
  if (going.length === 0) return false;
  return going.every((app) => addressReady(addresses[app] ?? EMPTY_ADDRESS, clusterDomain));
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
    out[app] = {
      hostname: hostnameFor(held.slug, clusterDomain),
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
 * A STARTING POINT IN AN EDITABLE FIELD, never a decision -- the same posture
 * `suggestSlug` takes, and for the same reason: somebody who has just pasted
 * a URL should not also have to invent a name, and somebody who wants a
 * different one is one keystroke away.
 */
export function suggestName(draft: ComposeDraft, zipTitle: string): string {
  if (draft.choice === "repo") {
    const parts = shortRepo(draft.repoUrl).split("/").filter((p) => p !== "");
    return parts[parts.length - 1] ?? "";
  }
  if (draft.choice === "zip") return zipTitle.replace(/\.zip$/i, "").trim();
  return "";
}

/** The address a new app starts with: its own name, slugified, or the source's. */
export function seedAddress(sourceName: string, app: string): AddressDraft {
  return { ...EMPTY_ADDRESS, slug: suggestSlug(sourceName, app) };
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
