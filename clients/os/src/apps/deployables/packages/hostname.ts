import { validateSlug } from "../hostname";

// The Packages surface reuses the app's OWN slug rules rather than restating
// them (`../hostname.ts`): the create-site form and the first-deploy picker are
// choosing the same kind of name for the same kind of row, and two copies of a
// mirrored policy is two chances to disagree with the Go that decides.
//
// What is genuinely new here is a SUGGESTION, which the create form has no use
// for -- somebody creating a site by hand already knows what to call it, while
// somebody deploying a package has just been handed several apps at once and
// should not have to invent a name for each.

export { hostnameFor } from "../hostname";

/**
 * A first suggestion for a deployable's address.
 *
 * Derived from the names already in front of the person -- the deployable's,
 * then the package's -- so the suggestion is recognisable rather than random.
 * It is a starting point in an editable field and never a decision: a hostname
 * is permanent for a site, and it belongs to whoever is deploying.
 *
 * Returns "" when nothing derivable is valid, which leaves the field empty and
 * the person typing. A suggestion that would be refused is worse than none.
 */
export function suggestSlug(packageName: string, deployableName: string): string {
  const candidates = [deployableName, packageName, `${deployableName}-${packageName}`]
    .map(slugify)
    .filter((c) => c !== "");
  for (const c of candidates) {
    if (validateSlug(c, "") === "") return c;
  }
  return "";
}

function slugify(raw: string): string {
  return raw
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40)
    .replace(/-+$/, "");
}
