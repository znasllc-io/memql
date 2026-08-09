// The composer's addresses.
//
// Same rule as the concept browser and the predefined views: every
// destination is a URL, not component state. Which concepts you are composing
// over lives in the query string, so a half-built composer is a link somebody
// can send; a saved view is a path, so it is a link somebody can bookmark.
//
// Concept ids are colon-delimited and go in the QUERY STRING here rather than
// in a path segment, because a composer takes SEVERAL of them and a repeated
// search parameter is the only honest way to carry a list. URLSearchParams
// handles the escaping, so this module does not repeat the concept browser's
// encodeSegment reasoning -- it only needs it for the saved view's id, which
// is one segment.

import { encodeSegment } from "../concepts/urls";

export const COMPOSE_ROOT = "/compose";

// The repeated parameter naming a concept to compose over. Repeated rather
// than comma-joined: a comma is legal inside an id and a splitter would be a
// second escaping scheme nobody documented.
export const CONCEPT_PARAM = "concept";

export function composePath(): string {
  return COMPOSE_ROOT;
}

// The composer, opened over a concept selection.
export function composeNewPath(conceptIds: readonly string[]): string {
  const params = new URLSearchParams();
  for (const id of conceptIds) params.append(CONCEPT_PARAM, id);
  const search = params.toString();
  return search === "" ? `${COMPOSE_ROOT}/new` : `${COMPOSE_ROOT}/new?${search}`;
}

// A saved view.
export function composedViewPath(viewId: string): string {
  return `${COMPOSE_ROOT}/${encodeSegment(viewId)}`;
}

// A saved view, reopened in the composer.
export function composedViewEditPath(viewId: string): string {
  return `${COMPOSE_ROOT}/${encodeSegment(viewId)}/edit`;
}

// readConceptSelection pulls the selection out of a location's search string.
// Duplicates are dropped and order is preserved -- the order is the section
// order of the composed view, and a repeated id would produce two identical
// sections.
export function readConceptSelection(search: string | URLSearchParams): string[] {
  const params = typeof search === "string" ? new URLSearchParams(search) : search;
  const out: string[] = [];
  for (const value of params.getAll(CONCEPT_PARAM)) {
    if (value !== "" && !out.includes(value)) out.push(value);
  }
  return out;
}

// The route patterns, so the route table and the builders above cannot drift.
// Relative, because ComposeRoutes is mounted under a splat.
export const COMPOSE_INDEX_PATTERN = "";
export const COMPOSE_NEW_PATTERN = "new";
export const COMPOSED_VIEW_PATTERN = ":viewId";
export const COMPOSED_VIEW_EDIT_PATTERN = ":viewId/edit";
