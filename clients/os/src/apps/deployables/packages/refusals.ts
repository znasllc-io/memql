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
    // NOT "could not be read". Five of this code's six sentences are about a
    // manifest that read perfectly and says something this cluster will not
    // accept -- no formatVersion, a formatVersion it does not know, no name,
    // a deployable with no name, two deployables sharing one name. Only the
    // YAML error is a reading failure, and "not valid" covers that one too.
    title: "The package manifest is not valid",
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
    // THE MANIFEST'S problem again, and only that. This code carried TWO
    // conditions until the engine split them: a storefront declared with no
    // binding, and an app with no hostname. One headline could describe
    // neither, so somebody who had simply not chosen an address was sent to
    // look at a Shopify binding that was never the problem. The second
    // condition is `deployable_hostname_unchosen` now, so this can name its
    // own again.
    title: "A storefront has no store to talk to",
    next: "The binding lives in the manifest, beside the deployable it belongs to.",
  },
  deployable_hostname_unchosen: {
    // THE OTHER HALF, and a placement problem rather than a manifest one: the
    // app is fine, nobody has said where it should live. The repair is on the
    // Where-it-lives stop, which is where the rail sends it.
    //
    // A person meets this by redeploying a source that declares an app they
    // have never deployed -- so the second sentence names the other way out,
    // which is usually the one they want.
    title: "A declared app has no address yet",
    next: "Choose one at Where it lives, or leave the app out and deploy the rest.",
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
    // The code covers the SOURCE and the BUILT OUTPUT, and COUNTS as well as
    // sizes: stages.go raises it for a built tree over the byte cap and for
    // one over the file-COUNT cap, and the build surface raises it for a
    // source too big to travel there at all. So the headline names neither
    // the object nor a byte size; the sentence names the limit that was hit
    // and, where there is one, the variable that raises it.
    title: "This is over the limits this cluster accepts",
    next: "",
  },
  source_unreadable: {
    // THE ENGINE'S CATCH-ALL, and this headline is the best of a bad set --
    // recorded here so the next reader knows it was examined rather than
    // overlooked. component/packages spells `source_unreadable` for roughly
    // two dozen conditions across seven files, and only about half are about
    // a source. The rest: "no package/site/deployment %q is readable by this
    // caller", "deployment %q belongs to a different package", "deployment %q
    // finished as %q rather than succeeded", "this cluster has no build
    // surface configured", "... no DSL staging surface ...", "... no rollout
    // surface ...".
    //
    // No single headline is true of all of those, and this build must supply
    // one: `SERVER_SENTENCE_ONLY` would be the honest home, and the coverage
    // gate in test/deployables/build.test.tsx requires COPY membership for
    // every engine code. So the headline stays with the reading the code is
    // DOCUMENTED for (refusal.go: "the fetched snapshot is not an archive
    // this engine can open at all"), which is also the reading the rail
    // commits to by parking this code on the Source stop. The durable repair
    // is engine-side: codes of their own for the conditions that are not
    // about a source.
    title: "This cluster could not read the source",
    next: "",
  },
  bundle_path_invalid: {
    // THE NEXT STEP, NOT A CAUSE. Five conditions share this code -- an entry
    // with an empty name, an absolute path, a NUL byte in the name, a path
    // escaping the package root, and a path this cluster will not read -- and
    // the line here used to assert the fourth for all five. The sentence
    // names the entry and what is wrong with it; the repair is the same
    // whichever it was.
    title: "This archive is not a plain tree",
    next: "Repack the tree as a plain directory and try again.",
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
    // TWO CONFIRMATIONS SHARE THIS CODE: archiving a deployable types its
    // HOSTNAME, archiving a package types its NAME. The headline used to say
    // "name" for both. The server's sentence quotes the exact string to type.
    title: "What you typed does not match",
    next: "",
  },
  deployable_build_failed: {
    title: "The build did not finish",
    next: "Every site is still serving what it was serving. The build output is below.",
  },
  deployable_build_timeout: {
    title: "The build ran out of time",
    // The two repairs, in the order somebody should try them. Naming the
    // variable matters: without it the only apparent option is "make the
    // build faster", which is not always possible.
    next: "Every site is still serving what it was serving. Make the build faster, or ask an operator to raise MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS.",
  },
  no_workbench_peer: {
    title: "This cluster has no build surface running",
    // Nothing here is the author's to fix, and saying so is the useful half:
    // otherwise somebody spends an afternoon on a build script that is fine.
    next: "Nothing was built and nothing was published. This is a cluster problem rather than one with your source -- an operator needs to look at the workbench.",
  },
  no_worker_available: {
    // NOT "cannot build this". Of the three sentences behind this code, one
    // is that no machine matched, one is that this cluster could not READ the
    // machines you have paired, and one is that the route exists and cannot
    // yet ask you for the per-run approval a computer-use act needs. In two
    // of the three the machines are perfectly capable. What is true in all
    // three is that none of them took the build.
    title: "None of your machines took this build",
    next: "",
  },
  deployment_abandoned: {
    title: "This cluster lost the node that was running it",
    // Said plainly, because the natural reading of a stopped deploy is that
    // it broke -- and this one did not.
    // "the same source" rather than "a fresh run", because that is what the
    // button next to this actually does: Retry deploys the bytes THIS run had
    // already fetched, so it deploys what this run was deploying rather than
    // whatever the branch has moved to since. The first draft said "a fresh
    // run" and contradicted its own control, which a browser caught by
    // putting the two side by side.
    next: "Nothing was published and nothing failed: every site is still serving what it was serving. Retry runs it again from the same source it had already fetched.",
  },
  snapshot_unavailable: {
    title: "There is nothing stored to retry from",
    next: "",
  },
  build_request_invalid: {
    title: "This cluster could not read the build request",
    next: "",
  },
  build_source_unreadable: {
    title: "The build surface could not read the source",
    next: "",
  },
  build_output_missing: {
    title: "The build finished and produced no files",
    next: "",
  },
  build_output_too_large: {
    title: "The built output is over the size this cluster accepts",
    next: "",
  },
  build_forward_failed: {
    title: "The build surface stopped answering mid-build",
    next: "Nothing was published. Retry starts a fresh run.",
  },
  build_entry_refused: {
    title: "The build surface refused this request",
    next: "",
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

  // -- the GitHub Connect epic (memql#4915): a credential that is an
  //    authorization GRANT rather than a pasted token. Each of these exists
  //    because the token vocabulary above answers the wrong question under a
  //    grant -- "private, or not there" is one 404 under a token and three
  //    distinct facts with three different repairs under a grant. The engine
  //    half lands in its own task; the copy is here first so the first
  //    refusal to reach a browser has a name. --

  reconnect_required: {
    // NOT an error tone anywhere it renders: nothing is broken and nothing
    // was lost. GitHub stopped honouring the grant -- the token expired, or
    // somebody revoked the authorization there -- and the repair is one
    // click with no typing.
    title: "Your GitHub connection needs renewing",
    next: "Reconnect GitHub in Settings > Sources. Nothing else changes.",
  },
  repository_not_installed: {
    // The grant is good and the person CAN see the repository; the app is
    // simply not installed on it. Saying "not found" here would send
    // somebody hunting for a permission problem that does not exist.
    title: "The app is not installed on that repository",
    next: "Install it on that repository at GitHub, then pick it again.",
  },
  installation_pending: {
    // The server's sentence NAMES THE ORGANISATION, and that name is the
    // whole useful content: the repair belongs to somebody else, and the
    // person's one next step is knowing whom to ask. A next step composed
    // here could only be a worse copy of it.
    title: "Waiting for an organization owner to approve",
    next: "",
  },
  github_app_not_configured: {
    // An OPERATOR's condition, not a person's. The sentence says what to do
    // instead AND who could change it, because a person reading this did
    // nothing wrong and cannot fix the cluster.
    title: "This cluster has no GitHub connection set up",
    next: "Paste a URL and a token instead, or ask an operator to set up the GitHub App.",
  },
  connect_state_invalid: {
    // A connect state is consumed exactly once, so this is the SECOND click
    // on a link as often as it is an expired one. Starting again costs
    // nothing, which is why the copy does not dwell on which it was.
    title: "That sign-in link is no longer valid",
    next: "Start again from Connect GitHub.",
  },

  // -- the lifecycle's fourth rung, and the stop button (epic memql#4937) --

  site_not_deletable: {
    // NAMES THE NEXT STEP, because "archive it first" is the whole answer. A
    // refusal that only said no would leave somebody looking for a control
    // that is deliberately not there yet.
    //
    // The headline says ARCHIVED rather than "still serving": delete runs
    // from `archived` or `draft`, so this refusal also meets somebody who has
    // ALREADY unpublished -- and telling them the deployable is still serving
    // when they just paused it reads as a fault in the cluster. The next step
    // makes the unpublish conditional for the same reason.
    title: "This deployable has not been archived yet",
    next: "Archive it first -- unpublish it if it is still serving -- and delete becomes available. Deleting is the end of the line, so it only runs from a state nothing is served from.",
  },
  delete_confirmation_mismatch: {
    title: "That is not this deployable's hostname",
    next: "Type it exactly as it appears above. The check is the server's, so nothing was written.",
  },
  site_system_owned: {
    // The person did nothing wrong and cannot fix this, so the copy explains
    // the rule rather than suggesting a repair that does not exist.
    title: "This is one of the cluster's own surfaces",
    next: "The portal and MemQL OS are re-seeded at every boot and are exempt from the lifecycle -- there is nothing to change here.",
  },
  deployment_not_cancellable: {
    // Two situations, one code, and the server's sentence says which: a run
    // that already finished, or one past the roll. Both mean "there is
    // nothing left to stop", which is what the headline says.
    title: "There is nothing left to stop",
    // CONDITIONAL, because only one of the two arms is about the roll: a run
    // that already finished is not restarting anything, and explaining the
    // roll to somebody who stopped a finished run describes a thing that did
    // not happen. The server's sentence says which arm this is.
    next: "If it is past the roll, this cluster is restarting onto its staged MemQL and stopping half way through is worse than letting it finish -- it will close on its own.",
  },
  deployment_cancelled: {
    // NOT A FAULT, and the copy leads with that. `cancelled` is somebody's own
    // decision, and reporting it back to them as a failure is the exact thing
    // the separate terminal status exists to prevent.
    title: "You stopped this deploy",
    next: "Nothing was published, and every deployable is still serving what it was serving. Deploy again whenever you are ready.",
  },
  deployable_skipped: {
    // Also not a fault: it is the record of a choice, and the whole point of
    // recording it is that a reader can tell it from a step that went missing.
    title: "You left this one out",
    next: "Nothing was built for it, and anything it already serves is untouched. Deploy it on its own whenever you want it.",
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
 *
 * NOTE the second gate. test/deployables/build.test.tsx runs its own coverage
 * assertion over `knownCodes()` ALONE, so a code moved here to say "the
 * sentence is the whole copy" fails that one. `source_unreadable` -- the
 * engine's catch-all, and the strongest candidate this table has -- is in
 * COPY for that reason, with the finding recorded on its entry.
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

/**
 * Codes that are somebody's NEXT STEP rather than a fault.
 *
 * `--os-warn`, NEVER `--os-error` (epic memql#4915's design plan states it
 * for the first two by name: "neither is a fault and both are somebody's
 * next step"). The other three are the same shape, and each one's own copy
 * above says so: the app is not installed on that repository yet, this
 * cluster never had a GitHub App, the link was already used. Nothing is
 * broken in any of them, and painting them in the fault colour teaches a
 * person to read the fault colour as decoration.
 *
 * It is a CLOSED list rather than a rule over the copy table, because "has a
 * next step" is true of half the package refusals and several of those --
 * MemQL that would stop a node booting -- are faults with a repair.
 */
const NOT_A_FAULT: ReadonlySet<string> = new Set([
  "reconnect_required",
  "installation_pending",
  "repository_not_installed",
  "github_app_not_configured",
  "connect_state_invalid",
]);

/**
 * The tone a refusal renders in.
 *
 * Read at the RENDER SITE rather than baked into `ProblemNotice`, because the
 * same component carries a build's own failures elsewhere in this app and
 * those are faults. An unknown code is an error: a fault nobody anticipated
 * is the one thing that must not be dressed as a next step.
 */
export function toneFor(code: string): "error" | "warn" {
  return NOT_A_FAULT.has(code.trim()) ? "warn" : "error";
}
