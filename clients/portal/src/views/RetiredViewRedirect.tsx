import { Navigate, useLocation, useParams } from "react-router-dom";
import type { ReactNode } from "react";

import { viewPath, viewRowPath } from "./urls";

// A renamed view's old address, pointed at its new one -- row segment, query
// and hash included (memql#4526).
//
// The precedent is RetiredSitesRedirect (src/deployables/DeployablesRoutes.tsx)
// and the reasoning is the same one altitude down: whoever bookmarked
// /views/people/rows/<id> did nothing wrong, and a Not Found would read as
// "that person is gone" when the VIEW was renamed.
//
// The ROW segment is carried rather than dropped, because the deep link is
// the one worth keeping: /views/people/rows/<id> is the page an admin holds
// open while changing somebody's role, and landing them on the list would
// make them find the row again. `rowId` arrives already decoded from the
// router, and viewRowPath re-encodes it -- src/concepts/urls.ts says so at
// decodeSegment, which is why there is no decode call here.
//
// TWO ROUTES PER RETIRED SLUG rather than one splat, and that is a
// router-ranking fact rather than a style preference. React Router scores a
// static segment above a dynamic one but PENALISES a splat, so `views/people/*`
// (18) loses to `views/:viewId/rows/:rowId` (26): a bookmarked row would have
// reached ViewPage with viewId="people" and rendered "No such view", from a
// redirect that looks perfectly correct in the diff.
export function RetiredViewRedirect({ to }: { to: string }): ReactNode {
  const { rowId = "" } = useParams<{ rowId: string }>();
  const location = useLocation();
  const target = rowId === "" ? viewPath(to) : viewRowPath(to, rowId);
  return <Navigate to={`${target}${location.search}${location.hash}`} replace />;
}
