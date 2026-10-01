// What this extension says about WHICH IMAGES a local cluster runs
// (memql#4246).
//
// A local cluster is in one of two lanes: released images pulled at a tag, or
// images built from the checkout on this machine. Every verb that moves it
// between them has to say so BEFORE it runs, because nothing else does -- the
// only other notice is the Deployments row afterwards, which stops naming a
// commit and starts naming a version, and a developer whose own edits quietly
// stopped running has no reason to go and look there.
//
// FOUR SURFACES SAY IT, AND THEY SAY IT ONCE. The install wizard's checklist,
// the upgrade confirmation, the Create-deployment tag screen and the rebuild
// checklist are each a place an operator decides whether to proceed. Four copies
// of the sentence would drift, and a drifted copy is a surface claiming the
// crossing is something slightly different from what the other three said.
//
// `rebuiltMessage` is here for a duller reason: it is pure wording that was
// marooned inside a `vscode`-importing panel, where the unit lane cannot reach
// it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4246

/**
 * The released images a cluster would be on, named when the tag is known.
 *
 * NO ADJECTIVE WHEN THERE IS NOTHING TO NAME. A placeholder produced "released
 * release images", which reads as a bug in the middle of a sentence asking an
 * operator to approve something.
 */
export function releasedImages(releasedTag: string): string {
  const tag = releasedTag.trim();
  return tag === "" ? "released images" : `released ${tag} images`;
}

/**
 * The one sentence every released-lane verb says over a checkout-mode cluster:
 * the operator's own build, which is the fact they are about to lose, and what
 * replaces it. The instance name is taken for the callers that have one and is
 * not said: the surfaces that say this are already about that cluster.
 */
export function returnsToReleasedImages(_instanceName: string, releasedTag: string): string {
  return `Your own build is replaced with ${releasedImages(releasedTag)}.`;
}

/**
 * What the operator is told when an update-and-rebuild lands.
 *
 * READ OFF BOTH ENVELOPES, and off nothing else -- the same rule `rebuiltMessage`
 * states, applied to the step that moved the checkout as well as the one that
 * built it. What the operator asked for is not evidence of what happened: an
 * update that found nothing to apply is a success, and reporting it as "brought
 * up to date" would be a claim the run did not make.
 *
 * THE OUTCOME IS READ, NOT INFERRED FROM THE COMMITS. `upToDate` and a
 * fast-forward of zero commits are indistinguishable by sha, and only one of
 * them is worth a sentence.
 */
export function updatedMessage(
  instanceName: string,
  update: Record<string, unknown> | undefined,
  rebuild: Record<string, unknown> | undefined,
): string {
  const str = (v: unknown): string => (typeof v === "string" ? v.trim() : "");
  const outcome = str(update?.outcome);
  const behind = update?.behind;
  const count =
    typeof behind === "number" && Number.isFinite(behind) && behind > 0
      ? `${String(behind)} new commit${behind === 1 ? "" : "s"}`
      : "the latest";
  const lead =
    outcome === "upToDate"
      ? "Already up to date"
      : outcome === "merged"
        ? "Merged the latest with your commits"
        : outcome === "fastForward"
          ? `Pulled ${count}`
          : // An outcome the envelope did not carry is left unnamed rather than
            // guessed at -- the same discipline as the dirtyCount guard below.
            "Pulled the latest";
  return `${lead}. ${rebuiltMessage(instanceName, rebuild)}`;
}

/**
 * What the operator is told when a rebuild lands: which build is running now.
 *
 * READ OFF THE ENVELOPE the step actually produced, never off what was asked
 * for: `k3d.dev` reports the commit it built from and how many files were
 * uncommitted at that moment, and those are facts about what is now running.
 *
 * A FACT THE ENVELOPE DID NOT CARRY IS LEFT OUT, NEVER INVENTED -- and the
 * `dirtyCount` guard is `typeof`, not a coercion, deliberately. `Number(
 * undefined)` is NaN, which prints "NaN uncommitted files"; `Number(null)` is
 * 0, a CLAIM that the tree was clean, made from a field never reported. A
 * reported zero says nothing either: a clean tree is the ordinary case.
 */
export function rebuiltMessage(
  instanceName: string,
  result: Record<string, unknown> | undefined,
): string {
  const str = (v: unknown): string => (typeof v === "string" ? v.trim() : "");
  const commit = str(result?.commit);
  const dirty = result?.dirtyCount;
  const parts = [
    ...(commit === "" ? [] : [commit.slice(0, 7)]),
    ...(typeof dirty === "number" && Number.isFinite(dirty) && dirty > 0
      ? [`${String(dirty)} uncommitted file${dirty === 1 ? "" : "s"}`]
      : []),
  ];
  const provenance = parts.length === 0 ? "" : ` (${parts.join(", ")})`;
  return `${instanceName} now runs your build${provenance}.`;
}

/**
 * The node types a rebuild built, named only when it was a PART of them.
 *
 * The script reports the EXPANDED list, so an empty request comes back as all
 * nine; listing nine names in a sentence tells the operator nothing they asked
 * about. A partial rebuild ("bff, agent") is worth saying, because the rest of
 * the cluster is still on the previous build.
 */
export function rebuiltNodes(
  requested: string,
  result: Record<string, unknown> | undefined,
): string {
  if (requested.trim() === "") return "";
  const nodes = typeof result?.nodes === "string" ? result.nodes.trim().split(/\s+/).filter((n) => n !== "") : [];
  return nodes.join(", ");
}
