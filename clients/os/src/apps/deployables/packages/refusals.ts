// What this cluster's refusal codes mean, said once.
//
// ===========================================================================
// KEYED BY CODE, RENDERED AS A SENTENCE, AND NEVER INVENTED
// ===========================================================================
// The engine's refusals are stable machine-readable codes
// (component/packages/refusal.go), and the OS renders a sentence for the ones
// it recognises. The rule that matters is the one the Deployables app already
// states for publish refusals: an error carrying NO known code keeps its OWN
// message. Inventing a friendly sentence for an unknown failure is how a real
// fault gets mistaken for somebody's mistake.
//
// Where the server's own sentence already names the specific thing -- which
// path was missing, which domain collided, which deployables are still serving
// -- it is rendered VERBATIM and this table supplies only the HEADLINE. A
// paraphrase would drop the one fact that helps.

export interface RefusalCopy {
  /** The headline: what happened, in the reader's terms. */
  title: string;
  /** What to do about it. Empty when the server's own sentence says. */
  next: string;
}

const COPY: Record<string, RefusalCopy> = {
  package_manifest_missing: {
    title: "This source has no package manifest",
    next: "A package is a tree with memql-package.yaml at its root, describing what to deploy. Add one and try again.",
  },
  package_manifest_invalid: {
    title: "The package manifest could not be read",
    next: "",
  },
  deployable_path_missing: {
    title: "A declared app is not in the source",
    next: "",
  },
  deployable_kind_unknown: {
    title: "A declared app has a kind this cluster does not serve",
    next: "",
  },
  deployable_binding_missing: {
    title: "A storefront has no store to talk to",
    next: "",
  },
  dsl_domain_reserved: {
    title: "This package ships MemQL under a name the engine owns",
    next: "",
  },
  dsl_refuses_boot: {
    title: "This package's MemQL would stop a node starting",
    next: "These are the same checks a node runs at boot, so nothing was deployed. Fix them in the source and deploy again.",
  },
  source_too_large: {
    title: "This source is over the size this cluster accepts",
    next: "",
  },
  source_unreadable: {
    title: "This cluster could not read the source",
    next: "",
  },
  bundle_path_invalid: {
    title: "This archive is not a plain tree",
    next: "One of its entries points outside the package root, so it was not built from a directory this cluster will read.",
  },
  go_pack_not_deployable: {
    title: "The Go pack was not deployed",
    next: "",
  },
  dsl_requires_cluster_owner: {
    title: "Deploying MemQL is a cluster owner's decision",
    next: "",
  },
  package_has_active_deployables: {
    title: "This package still has sites that are serving",
    next: "",
  },
  archive_confirmation_mismatch: {
    title: "That name does not match",
    next: "",
  },
  deployable_build_failed: {
    title: "The build did not finish",
    next: "Every site is still serving what it was serving. The build output is below.",
  },
  deployable_publish_failed: {
    title: "The site could not be published",
    next: "",
  },
  deploy_failed: {
    // The pipeline's own code for a fault that is not a refusal -- a store or
    // a storage call that failed mid-run. The server's sentence is the error
    // itself, and the honest next step is the one the append-only rule
    // prescribes everywhere else: a new attempt.
    title: "This deploy did not finish",
    next: "Nothing about the source was changed. Deploy again to start a fresh attempt.",
  },

  // -- the compose epic (memql#4885): personal source credentials and the
  //    target model. The engine half lands beside these; the copy is here
  //    first so the first refusal to reach a browser has a name. --

  credential_not_found: {
    title: "This source's credential is not one you can use",
    next: "Pick one of your own credentials on the Source stop, or add a new one there.",
  },
  credential_revoked: {
    title: "This source's credential was revoked",
    next: "Switch the source to another credential on its Source stop.",
  },
  source_host_unsupported: {
    title: "Only github.com today",
    next: "Paste a github.com URL, or upload the tree as a zip in Files.",
  },
  deployable_target_not_offered: {
    // The server's sentence names the kind ("iOS is not offered on this
    // cluster yet"); it is rendered on the What-it-is stop verbatim and the
    // rest of the package deploys, so there is no next step to give.
    title: "That kind is not offered on this cluster yet",
    next: "",
  },

  // -- the two PLACEMENT halves (memql#4887). Both say the deploy SUCCEEDED
  //    and one optional half of the address did not. The pipeline applies
  //    them after the publish, under the caller's own actor, and records the
  //    guard's refusal on the outcome rather than failing the run -- so a
  //    headline reading as a failed deploy would be the opposite of what
  //    happened, and would send somebody looking for a site that is already
  //    serving. The server's sentence, beneath, names the specific guard. --

  deployable_account_refused: {
    title: "It is live, but not tied to that client",
    next: "The deployable is serving at its address. Set the client on its Where it lives stop.",
  },
  deployable_domain_refused: {
    title: "It is live at its cluster address, but the domain was not bound",
    next: "The deployable is serving. Add the domain again on its Where it lives stop, where the two DNS records are.",
  },
};

/**
 * Codes the engine emits for which the server's sentence IS the whole copy:
 * no headline this build could add would say more than the sentence does.
 *
 * Empty today, and kept so the coverage test (test/deployables/refusals.test.ts)
 * has a second acceptable home for a code: every code the engine can emit
 * must be in COPY or here, and a code in neither renders under the neutral
 * heading -- the designed fallback for a fault nobody anticipated, not a
 * place to leave a known one.
 */
export const SERVER_SENTENCE_ONLY: readonly string[] = [];

/**
 * copyFor returns the headline and next step for a code, or null when this
 * cluster has said something this build does not have a name for.
 *
 * A null result is the signal to render the server's message ALONE, under a
 * neutral heading -- never under a guessed one.
 */
export function copyFor(code: string): RefusalCopy | null {
  return COPY[code.trim()] ?? null;
}

/** Every code this build renders copy for. Exported for the coverage test. */
export function knownCodes(): string[] {
  return Object.keys(COPY).sort();
}
