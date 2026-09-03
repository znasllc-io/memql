import { PROBE_REASONS } from "../page/rail";
import { copyFor } from "../packages/refusals";

// The two compose probes, as the Source stop reads them (epic memql#4885,
// design section D and H).
//
// ===========================================================================
// A REASON ABOUT THE SOURCE PARKS. A REASON ABOUT THE PROBE DOES NOT.
// ===========================================================================
// `sourceProbe` answers a TYPED reason, never GitHub's own body, and the
// design is explicit about what the answer is worth: "the fetch is the
// authority and the probe is a courtesy" (design H). So the seven reasons
// split in two, and the split is the whole of this module:
//
//   * `not_found_or_private`, `credential_cannot_see_it`,
//     `credential_not_found`, `credential_revoked` and
//     `source_host_unsupported` are definite answers ABOUT THE REPOSITORY.
//     Analyzing anyway would open a run, fetch, and refuse with the same
//     information one round trip later -- so they park the Source stop and
//     Analyze stays out of reach until the answer changes.
//   * `rate_limited`, and a thrown ERROR, are answers about the PROBE. They
//     say so and leave the field editable, and Analyze stays reachable,
//     because blocking a deploy of a public repository on a probe that could
//     not run is exactly the failure design H names.
//
// Everything here is pure -- a reply in, a sentence or a boolean out -- so
// what the stop SAYS for each reason is asserted against this module rather
// than through three layers of render.

/** What `sourceProbe` answers with. `private` and `defaultBranch` mean something only when `reachable`. */
export interface SourceProbeReply {
  host: string;
  reachable: boolean;
  private: boolean;
  defaultBranch: string;
  reason: string;
  /**
   * Every branch the repository has, DEFAULT FIRST, answered only under a
   * grant (epic memql#4915). It fills the ref picker, so a person chooses a
   * branch that exists instead of typing one that does not. Empty is the
   * ordinary answer for a pasted-token probe and the ref stays a text field.
   */
  branches: string[];
  /**
   * What memql-package.yaml says about itself, read through the contents API
   * before anything is fetched. A PREVIEW: the What-it-is stop shows it so a
   * person recognises the package they picked, and Analyze over the real
   * snapshot remains the authority on every question it answers.
   */
  manifest: ManifestSummary;
}

/**
 * The manifest preview.
 *
 * EMPTY IS A VALID ANSWER AND NEVER A COMPLAINT. A repository with no
 * manifest, one that does not parse, one written for a format version this
 * cluster does not read -- all answer an empty summary, and the OS renders
 * no preview and says nothing. The engine decided that (`probeManifest`) and
 * this side must not second-guess it: a warning here would report a manifest
 * problem twice, in two sentences, before the run that actually reads the
 * tree has said anything at all.
 */
export interface ManifestSummary {
  name: string;
  deployables: ManifestDeployable[];
  /** The directory names directly under `dsl/`, which IS the declaration. */
  dslDomains: string[];
}

/** One declared deployable, in the three facts the preview shows. */
export interface ManifestDeployable {
  name: string;
  kind: string;
  path: string;
}

export const EMPTY_MANIFEST: ManifestSummary = { name: "", deployables: [], dslDomains: [] };

/** Whether a summary has anything to show. An empty one gets no preview. */
export function manifestIsEmpty(manifest: ManifestSummary): boolean {
  return manifest.name.trim() === "" && manifest.deployables.length === 0 && manifest.dslDomains.length === 0;
}

/**
 * A member list, whether it arrives as a decoded array or as JSON text.
 *
 * The same two shapes `sources/repositories.ts` reads, for the same reason: a
 * scalar on a builtin's reply row can cross the wire as text, and a nested
 * list arrives decoded. An UNREADABLE list is no list -- never a thrown parse
 * error, because one malformed member must not cost the ref picker its
 * branches or take the whole stop down with it.
 */
function membersOf(value: unknown): unknown[] {
  if (Array.isArray(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    try {
      const parsed: unknown = JSON.parse(value);
      if (Array.isArray(parsed)) return parsed;
    } catch {
      // Answered below as no members.
    }
  }
  return [];
}

function objectOf(value: unknown): Record<string, unknown> | null {
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  if (typeof value === "string" && value.trim() !== "") {
    try {
      const parsed: unknown = JSON.parse(value);
      if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>;
      }
    } catch {
      // Answered below as no manifest, which renders no preview.
    }
  }
  return null;
}

function textOf(value: unknown, key: string): string {
  if (value === null || typeof value !== "object") return "";
  const held = (value as Record<string, unknown>)[key];
  return typeof held === "string" ? held : "";
}

/**
 * The branch names a probe answered, ORDER PRESERVED.
 *
 * The order IS the answer: the engine puts the default branch first
 * (`probeBranches`), and re-sorting here would quietly drop the one fact the
 * ref picker is supposed to lead with. A member that is not a string is
 * dropped rather than coerced -- an empty option is one somebody picks by
 * mistake.
 */
export function branchNamesFrom(value: unknown): string[] {
  return membersOf(value).filter((m): m is string => typeof m === "string" && m.trim() !== "");
}

/**
 * The manifest preview, from whichever shape the reply carried.
 *
 * Every field is NAMED rather than cast, which is what keeps a field added
 * engine-side from arriving in this app unconsidered. A deployable with no
 * name is dropped: the preview keys on it, and a row that says nothing about
 * what it is has nothing to preview.
 */
export function manifestFrom(value: unknown): ManifestSummary {
  const row = objectOf(value);
  if (row === null) return { ...EMPTY_MANIFEST };
  const deployables: ManifestDeployable[] = [];
  for (const member of membersOf(row["deployables"])) {
    const name = textOf(member, "name").trim();
    if (name === "") continue;
    deployables.push({ name, kind: textOf(member, "kind"), path: textOf(member, "path") });
  }
  return {
    name: typeof row["name"] === "string" ? row["name"] : "",
    deployables,
    dslDomains: branchNamesFrom(row["dslDomains"]),
  };
}

/** What `artifactProbe` answers with, over the caller's own zip. */
export interface ArtifactProbeReply {
  isPackage: boolean;
  isBuiltSite: boolean;
  fileCount: number;
  totalBytes: number;
}

/**
 * The reasons the engine names, in the order the design lists them.
 *
 * The last two arrived with the GRANT (epic memql#4915) and are why this list
 * is not seven: `sourceProbe` under a connection can answer that GitHub
 * refused the grant itself, or that the app is not installed on this
 * repository. Both are DEFINITE answers about whether this cluster can read
 * that repository, so both park -- see `probeParks`.
 */
export const PROBE_REASON_CODES = [
  "ok",
  "not_found_or_private",
  "credential_cannot_see_it",
  "credential_not_found",
  "credential_revoked",
  "source_host_unsupported",
  "rate_limited",
  "reconnect_required",
  "repository_not_installed",
] as const;

export type ProbeReason = (typeof PROBE_REASON_CODES)[number];

/** GitHub is the only host this cluster fetches from today. */
export const SOURCE_HOST = "github.com";

const RATE_LIMITED =
  "GitHub is rate-limiting this cluster right now, so it could not answer. Deploying still works -- the fetch asks again.";

/**
 * The stop's own sentence for a reply, in the design's fixed words where it
 * fixes them.
 *
 * `ok` answers what a person wants to know NEXT: a public repository says
 * which branch will be followed, a private one says the credential is
 * working. The two credential codes reuse the refusal table's headline
 * rather than restating it -- the same code arriving from a real fetch
 * renders that exact sentence, and two spellings of one refusal is two
 * things to keep in step.
 */
export function probeNote(reply: SourceProbeReply): string {
  switch (reply.reason) {
    case "ok":
      return reply.private
        ? `private, and reachable under this credential -- default branch ${branchOf(reply)}`
        : `public, default branch ${branchOf(reply)}`;
    case "not_found_or_private":
      return PROBE_REASONS.notReachable;
    case "credential_cannot_see_it":
      return PROBE_REASONS.credentialCannotSee;
    case "source_host_unsupported":
      return PROBE_REASONS.hostUnsupported;
    case "rate_limited":
      return RATE_LIMITED;
    case "credential_not_found":
    case "credential_revoked":
    // The two grant reasons take the SAME route for the same reason: the
    // refusal table already holds their headline, the identical code arriving
    // from a real fetch renders that exact sentence, and two spellings of one
    // refusal is two things to keep in step.
    case "reconnect_required":
    case "repository_not_installed":
      return copyFor(reply.reason)?.title ?? "";
    default:
      // A reason this build has no name for is not paraphrased. The stop
      // renders nothing rather than a guess, and Analyze stays reachable --
      // the same posture an unknown refusal code gets everywhere else.
      return "";
  }
}

/** Whether the answer is about the repository, and therefore parks the flow. */
export function probeParks(reason: string): boolean {
  return (
    reason === "not_found_or_private" ||
    reason === "credential_cannot_see_it" ||
    reason === "credential_not_found" ||
    reason === "credential_revoked" ||
    reason === "source_host_unsupported" ||
    // A LAPSED GRANT AND AN UNINSTALLED REPOSITORY PARK TOO (memql#4915).
    // Each is a definite answer about this repository with a repair that is
    // one click away -- reconnect, or install the app on it -- so analyzing
    // anyway would open a run, fetch, and refuse with the same information
    // one round trip later. `github_app_not_configured` is deliberately NOT
    // here: it is an operator's condition, the token path still works, and
    // parking on it would block a deploy this cluster can perform.
    reason === "reconnect_required" ||
    reason === "repository_not_installed"
  );
}

/**
 * Whether the credential field appears.
 *
 * It appears the moment GitHub answers "404 or private" with no credential --
 * that is what the field is FOR -- and stays while a chosen credential is the
 * thing that is wrong. It never appears for an unsupported host: no token
 * makes github.com out of a gitlab URL.
 */
export function probeWantsCredential(reason: string): boolean {
  return (
    reason === "not_found_or_private" ||
    reason === "credential_cannot_see_it" ||
    reason === "credential_not_found" ||
    reason === "credential_revoked"
  );
}

function branchOf(reply: SourceProbeReply): string {
  return reply.defaultBranch.trim() === "" ? "the default" : reply.defaultBranch.trim();
}

// ---------------------------------------------------------------------------
// The zip
// ---------------------------------------------------------------------------

/** Which of the two paths a probed zip takes, or neither. */
export type ZipVerdict = "package" | "built_site" | "neither";

/**
 * A zip that is BOTH is a package -- the engine's own reading
 * (`artifactProbe`: "both at the root is a package"), mirrored here rather
 * than re-decided, so the OS never offers the hand-made path for a tree the
 * pipeline would analyze as a package.
 */
export function zipVerdict(reply: ArtifactProbeReply): ZipVerdict {
  if (reply.isPackage) return "package";
  if (reply.isBuiltSite) return "built_site";
  return "neither";
}

/**
 * What a zip that is neither IS, said in facts rather than in a diagnosis.
 *
 * The commonest cause is a folder wrapping the site one level down, and the
 * engine's own capability description says so -- but this build cannot tell
 * that from an empty archive or a source tree, so it reports what it counted
 * and names the likely cause as a possibility rather than as a finding.
 */
export function zipUnusableNote(reply: ArtifactProbeReply): string {
  const files = `${reply.fileCount} ${reply.fileCount === 1 ? "file" : "files"}`;
  return (
    `This zip holds ${files}, and neither memql-package.yaml nor index.html is at its root. ` +
    "A zip that wraps its site in a folder one level down looks like this; so does a source tree that has not been built."
  );
}
