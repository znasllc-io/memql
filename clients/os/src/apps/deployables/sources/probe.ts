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
}

/** What `artifactProbe` answers with, over the caller's own zip. */
export interface ArtifactProbeReply {
  isPackage: boolean;
  isBuiltSite: boolean;
  fileCount: number;
  totalBytes: number;
}

/** The seven reasons the engine names, in the order the design lists them. */
export const PROBE_REASON_CODES = [
  "ok",
  "not_found_or_private",
  "credential_cannot_see_it",
  "credential_not_found",
  "credential_revoked",
  "source_host_unsupported",
  "rate_limited",
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
    reason === "source_host_unsupported"
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
