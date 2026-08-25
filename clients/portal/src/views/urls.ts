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
// A view id is a short slug this repo chooses ("users", "deployments"), not a
// concept id, so it needs no encoding of its own. A ROW id does -- row ids are
// colon-punctuated exactly like concept ids -- so it goes through the same
// encodeSegment the browser uses rather than a second copy of that reasoning.

import { encodeSegment } from "../concepts/urls";

// The route SEGMENT, without a leading slash, and the absolute ROOT built
// from it. Route paths inside the AppShell are relative while every address
// this module hands out is absolute, so the two spellings come from one place
// -- otherwise a redirect mounts at a depth the builders never address, which
// is a 404 nothing in the diff points at.
export const VIEWS_ROUTE_SEGMENT = "views";

export const VIEWS_ROOT = `/${VIEWS_ROUTE_SEGMENT}`;

export function viewPath(viewId: string): string {
  return `${VIEWS_ROOT}/${viewId}`;
}

export function viewRowPath(viewId: string, rowId: string): string {
  return `${VIEWS_ROOT}/${viewId}/rows/${encodeSegment(rowId)}`;
}

// The route PATTERNS, so routes.tsx and the builders above cannot drift.
export const VIEW_ROUTE_PATTERN = `${VIEWS_ROUTE_SEGMENT}/:viewId`;
export const VIEW_ROW_CHILD_PATTERN = "rows/:rowId";

// THE SLUGS THESE VIEWS USED TO HAVE, and where they went (memql#4526).
//
// The two directory views took their concepts' names -- `people` became
// `users` and `customers` became `accounts` -- and the old addresses are
// REDIRECTED rather than 404'd. That is the precedent RetiredSitesRedirect
// set one surface over: whoever bookmarked /views/people did nothing wrong,
// and a Not Found reads as "the capability is gone" when it was renamed.
//
// Kept as DATA here rather than as hand-written <Route> lines in the route
// table, because the {old, new} pair IS the fact and a redirect naming the
// wrong destination stays invisible until somebody follows a two-year-old
// bookmark.
export const RETIRED_VIEW_IDS: Readonly<Record<string, string>> = {
  people: "users",
  customers: "accounts",
};
