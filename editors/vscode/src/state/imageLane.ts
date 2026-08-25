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
 * The one sentence every released-lane verb says over a checkout-mode cluster.
 *
 * Names the INSTANCE rather than "the cluster", because the surfaces that say
 * it are looking at a named row, and says what it is running today, because
 * that is the fact the operator is about to lose.
 */
export function returnsToReleasedImages(instanceName: string, releasedTag: string): string {
  return `This returns ${instanceName} to ${releasedImages(
    releasedTag,
  )}; it runs a checkout build today.`;
}

/**
 * What the operator is told when a rebuild lands.
 *
 * READ OFF THE ENVELOPE the step actually produced, never off what was asked
 * for: `k3d.dev` reports the nodes it built, the commit it built them from and
 * how many files were uncommitted at that moment, and those are facts about
 * what is now running. Restating the request would print nothing at all for the
 * ordinary case, where an empty list expands to nine node types.
 *
 * A FACT THE ENVELOPE DID NOT CARRY IS LEFT OUT, NEVER INVENTED -- and the
 * `dirtyCount` guard is `typeof`, not a coercion, deliberately. `Number(
 * undefined)` is NaN, which prints "NaN uncommitted files"; `Number(null)` is
 * 0, which prints "0 uncommitted files" and is far worse: it is a CLAIM that
 * the tree was clean, made from a field that was never reported.
 */
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
        ? "Combined the latest with your own commits"
        : outcome === "fastForward"
          ? `Brought your checkout up to date with ${count}`
          : // An outcome the envelope did not carry is left unnamed rather than
            // guessed at -- the same discipline as the dirtyCount guard below.
            "Updated your checkout";
  return `${lead}. ${rebuiltMessage(instanceName, rebuild)}`;
}

export function rebuiltMessage(
  instanceName: string,
  result: Record<string, unknown> | undefined,
): string {
  const str = (v: unknown): string => (typeof v === "string" ? v.trim() : "");
  // The script reports the EXPANDED list, space-separated.
  const built = str(result?.nodes)
    .split(/\s+/)
    .filter((node) => node !== "");
  const nodes = built.length === 0 ? "the app nodes" : built.join(", ");
  const commit = str(result?.commit);
  const dirty = result?.dirtyCount;
  const parts = [
    ...(commit === "" ? [] : [commit.slice(0, 7)]),
    ...(typeof dirty === "number" && Number.isFinite(dirty)
      ? [`${String(dirty)} uncommitted file${dirty === 1 ? "" : "s"}`]
      : []),
  ];
  const provenance = parts.length === 0 ? "" : ` (${parts.join(", ")})`;
  return `Rebuilt ${nodes} -- ${instanceName} now runs your checkout${provenance}.`;
}
