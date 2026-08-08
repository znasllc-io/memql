// The predefined views' addresses.
//
// Same rule as the concept browser (src/concepts/urls.ts): every destination
// is a URL, not component state, so a view -- and a row within it -- is a link
// someone can paste into a ticket.
//
// The shape mirrors the browser's deliberately, because the two are the same
// gesture at different altitudes:
//
//   /concepts/:conceptId/rows/:rowId    any concept, generically rendered
//   /views/:viewId/rows/:rowId          a designed view of one concept
//
// A view id is a short slug this repo chooses ("people", "deployments"), not a
// concept id, so it needs no encoding of its own. A ROW id does -- row ids are
// colon-punctuated exactly like concept ids -- so it goes through the same
// encodeSegment the browser uses rather than a second copy of that reasoning.

import { encodeSegment } from "../concepts/urls";

export const VIEWS_ROOT = "/views";

export function viewPath(viewId: string): string {
  return `${VIEWS_ROOT}/${viewId}`;
}

export function viewRowPath(viewId: string, rowId: string): string {
  return `${VIEWS_ROOT}/${viewId}/rows/${encodeSegment(rowId)}`;
}

// The route PATTERNS, so routes.tsx and the builders above cannot drift.
export const VIEW_ROUTE_PATTERN = "views/:viewId";
export const VIEW_ROW_CHILD_PATTERN = "rows/:rowId";
