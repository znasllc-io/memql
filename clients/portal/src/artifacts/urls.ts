// Addresses inside the artifacts surface. Mirrors sites/urls.ts: every
// destination is a URL, not a piece of component state (#3316) -- including
// the active label filter, which lives in the query string rather than in a
// useState the back button cannot see. Paths are written WITHOUT the /portal
// prefix since the router's basename already carries it (see App / Vite's
// `base`).

export const ARTIFACTS_ROOT = "/artifacts";

// The active label filter's query-string key ("here are the artifacts
// labelled X" is itself a link). One named export because both the list
// page (which writes it via useSearchParams) and its test have to agree on
// the spelling.
export const LABEL_PARAM = "label";

// The meaning-search query, in the URL for the same reason the label filter
// is: a search someone ran is a link they can send, it survives a refresh,
// and the back button leaves the search rather than un-typing it one
// character at a time. The two COMPOSE -- ?q=budget&label=finance is
// "artifacts about budgets, among the ones labelled finance" -- which is why
// each is its own key rather than one packed mode parameter.
export const SEARCH_PARAM = "q";

// Whether archived rows are shown. In the URL rather than in a useState
// because it changes WHICH ROWS the page is about, and every other such
// choice on this page is addressable.
export const ARCHIVED_PARAM = "archived";
export const ARCHIVED_VALUE = "1";

export function artifactsPath(): string {
  return ARTIFACTS_ROOT;
}

// artifactPath addresses one artifact's detail + label editor. Artifact ids
// are derived deterministically server-side (concat("artifact-",
// hash(sourceConceptRef)), dsl/library/mutations.memql) -- a plain opaque
// string with no colon, same shape as a site's newShortId() (see
// docs/public/concepts/identifiers.md on the bare-ids client contract), so a
// plain encodeURIComponent is enough; unlike a concept row id there is no
// `concept:id` punctuation to preserve through the round trip.
export function artifactPath(artifactId: string): string {
  return `${ARTIFACTS_ROOT}/${encodeURIComponent(artifactId)}`;
}
