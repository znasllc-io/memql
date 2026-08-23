import { Navigate, Route, Routes, useLocation, useParams } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { DeployableDetailPage } from "./DeployableDetailPage";
import { DeployablesPage } from "./DeployablesPage";
import { DEPLOYABLES_ROOT } from "./urls";

// Deployables (memql#4346) owns everything under /deployables. Mounted from
// the route table as a SPLAT (`deployables/*`), the same convention
// ArtifactsRoutes and IntegrationsRoutes use and for the same reason: a change
// here does not need an edit to the shared route table.
//
// TWO ADDRESSES:
//
//   /deployables          what this cluster hosts for you, plus the create form
//   /deployables/:siteId  one deployable's deploy / rollback / status / delete
//
// The detail screen is a CHILD ADDRESS rather than a modal -- the standard the
// whole portal is held to (#3316): a deployable somebody is about to roll back
// is a link they can send a colleague, and a page that survives a refresh.
export function DeployablesRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<DeployablesPage />} />
      <Route path=":siteId" element={<DeployableDetailPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}

// /sites/* -> /deployables/*, TAIL AND ALL.
//
// Redirected rather than 404'd, the precedent the retired /cluster-ops route
// set in routes.tsx: whoever bookmarked a site's detail page did nothing wrong,
// and a Not Found would read as "the capability is gone" when it was renamed.
//
// The tail is carried because the useful bookmark is the DEEP one --
// /sites/:siteId is the page an operator keeps open while rolling something
// back, and dropping them on the list would make them find the row again. The
// query string and hash ride along for the same reason; nothing on this surface
// uses them today, and a redirect that silently ate them would be a trap for
// whatever does first.
export function RetiredSitesRedirect(): ReactNode {
  const params = useParams();
  const location = useLocation();
  const tail = params["*"] ?? "";
  const to = tail === "" ? DEPLOYABLES_ROOT : `${DEPLOYABLES_ROOT}/${tail}`;
  return <Navigate to={`${to}${location.search}${location.hash}`} replace />;
}
