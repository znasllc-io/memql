// The extension's own commit against the checkout it drives (memql#5076).
//
// A SECOND SKEW AXIS, and it is not the one skewHint.ts is about. That one
// compares the extension against the CLUSTER's recorded version and appends a
// hint to a transport failure. This one compares the extension against the
// CHECKOUT on disk -- the tree whose scripts a rebuild runs -- and it is not a
// hint at all: it is a fact, stated where the operator is already looking.
//
// The split is real and it is what makes the skew invisible. Since memql#5056
// and memql#5064, an action that BUILDS from the checkout reads the CHECKOUT's
// scripts, so the build recipe matches the tree. Everything the extension does
// with its OWN code -- the install graph documents, the plan functions, the
// preflight, the panel -- is still whatever it was packaged with. So the two
// halves of one operation can come from two commits, correctly, and nothing
// said so.
//
// WORDING IS COMPOSED HERE, ONCE, for skewHint.ts's stated reason: two surfaces
// report this and a sentence written twice is a sentence that disagrees.
//
// NO DIRECTION IS CLAIMED. "Ahead" and "behind" would need the checkout's
// history, which this module does not have and will not guess at -- and the
// direction is not what the operator needs anyway. What they need is that the
// two are not the same code.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/** A commit rendered the way every other surface in this extension renders one. */
export function shortCommit(commit: string): string {
  return commit.trim().slice(0, 7);
}

export interface CheckoutSkewInputs {
  /** The commit this extension was packaged from, or "" when unstamped. */
  extensionCommit: string | undefined;
  /** Whether that build had uncommitted edits on top of its commit. */
  extensionDirty?: boolean;
  /** The commit the install's `stackCheckout` step recorded, or "". */
  checkoutCommit: string | undefined;
}

export type CheckoutSkewState =
  /** Both commits are known and identical. */
  | "same"
  /** Both are known and they differ. */
  | "diverged"
  /** One side is not recorded, so nothing can be compared. */
  | "unknown"
  /** There is no checkout to compare against. */
  | "noCheckout";

export interface CheckoutSkew {
  state: CheckoutSkewState;
  /**
   * The full sentence, for a surface with room to explain -- the preflight,
   * where the operator is about to act and the consequence lands.
   */
  sentence: string;
  /**
   * The same fact at a glance, for a key/value row on a page somebody is
   * BROWSING.
   *
   * TWO REGISTERS, DELIBERATELY, and the long one in a facts grid was the first
   * draft's mistake: it sat beside `version v0.19.1` and `domain
   * memql.localhost` as a two-line paragraph and broke the rhythm of the list
   * it belongs to. The issue's own two tiers are "say it" and "say it when it
   * matters"; collapsing them to one wording makes the browsing surface heavy
   * and the acting surface no clearer.
   */
  terse: string;
}

/**
 * Compares the two commits and says what it found.
 *
 * `noCheckout` is separated from `unknown` because they send a reader to
 * different places: nothing to compare against is not the same as something
 * this build cannot tell you about, and collapsing them would put "cannot tell"
 * in front of every remote instance, which has no checkout by construction.
 */
export function checkoutSkew(i: CheckoutSkewInputs): CheckoutSkew {
  const checkout = (i.checkoutCommit ?? "").trim();
  const extension = (i.extensionCommit ?? "").trim();

  if (checkout === "") {
    return {
      state: "noCheckout",
      sentence: "This install has no recorded checkout, so there is nothing to compare against.",
      terse: "",
    };
  }
  if (extension === "") {
    return {
      state: "unknown",
      // The honest reading, and the common one: an extension running out of a
      // checkout in the Extension Development Host was never packaged.
      sentence:
        `This extension records no build commit (it was run from source rather than packaged), ` +
        `so it can't be compared with the checkout at ${shortCommit(checkout)}.`,
      terse: `not recorded, so it cannot be compared with ${shortCommit(checkout)} in the checkout`,
    };
  }
  if (extension === checkout && !i.extensionDirty) {
    return {
      state: "same",
      sentence: `Built at ${shortCommit(extension)}, the same commit as the checkout.`,
      terse: `${shortCommit(extension)}, the same commit as the checkout`,
    };
  }
  // A DIRTY BUILD IS DIVERGED even when the commits match, and that is not
  // pedantry: an extension built from uncommitted edits is not the commit it
  // names, so "the same commit as the checkout" would be false in exactly the
  // situation a developer is most likely to be debugging.
  if (extension === checkout) {
    return {
      state: "diverged",
      sentence:
        `Built from uncommitted edits on ${shortCommit(extension)}, the checkout's commit, ` +
        `so this extension isn't exactly that commit.`,
      terse: `${shortCommit(extension)} plus uncommitted edits, which the checkout does not have`,
    };
  }
  return {
    state: "diverged",
    sentence:
      `Built at ${shortCommit(extension)}; the checkout is at ${shortCommit(checkout)}. ` +
      `A build from the checkout runs the checkout's scripts, while its steps and this page ` +
      `come from this extension.`,
    terse: `${shortCommit(extension)}, and the checkout is at ${shortCommit(checkout)}`,
  };
}

/**
 * The one-line form for a facts row, or "" when there is nothing to say.
 *
 * "" for `noCheckout` deliberately: a remote instance has no checkout and a row
 * saying so on every one of them is the noise that makes the row that matters
 * unreadable -- rebuildPreflight.ts states that rule about its own lane line,
 * and it is the same rule.
 */
export function checkoutSkewFactValue(i: CheckoutSkewInputs): string {
  return checkoutSkew(i).terse;
}
