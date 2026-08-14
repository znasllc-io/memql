import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { SiteDetailPage } from "./SiteDetailPage";
import { SitesPage } from "./SitesPage";

// Site management (memql#3717) owns everything under /sites. Mounted from
// the route table as a SPLAT (`sites/*`), the same convention
// IntegrationsRoutes uses and for the same reason (routes.tsx:82-91): a
// change here does not need an edit to the shared route table.
//
// TWO ADDRESSES:
//
//   /sites          every site in the cluster, live, plus the create form
//   /sites/:siteId  one site's publish / rollback / status / delete actions
//
// The detail screen is a CHILD ADDRESS rather than a modal -- the standard
// the whole portal is held to (#3316): a site an operator is about to roll
// back is a link they can send a colleague, and a page that survives a
// refresh.
export function SitesRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<SitesPage />} />
      <Route path=":siteId" element={<SiteDetailPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
