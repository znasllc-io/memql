import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { AppSessionPage } from "./AppSessionPage";
import { LocalAppsPage } from "./LocalAppsPage";
import { MachinesPage } from "./MachinesPage";
import { WorkbenchesPage } from "./WorkbenchesPage";
import { fleetPath, SESSION_ROUTE_PATTERN } from "./urls";

// The Fleet (epic memql#4349) owns everything under /fleet.
//
// Mounted from the route table as a SPLAT (`fleet/*`), the convention
// AdminRoutes / SitesRoutes / IntegrationsRoutes use and for the same reason:
// a surface added here needs no edit to the shared route table, which in a
// worktree three agents are working at once is three chances to clobber each
// other's line.
//
// TWO ADDRESSES:
//
//   /fleet/machines      the computers registered to this cluster as workers
//   /fleet/apps          delegating work to a local app on somebody's machine
//   /fleet/apps/sessions/:id   one delegated run's live transcript
//   /fleet/workbenches   the cluster's own sandboxed working directories
//
// /fleet itself redirects to Machines rather than rendering a landing page.
// The lesson /admin's index learned in memql#4264 is that an overview above
// two destinations is a third door to the same two things; the tab strip in
// FleetFrame already is the index.
export function FleetRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<Navigate to={fleetPath("machines")} replace />} />
      <Route path="machines" element={<MachinesPage />} />
      <Route path="apps" element={<LocalAppsPage />} />
      <Route path={SESSION_ROUTE_PATTERN} element={<AppSessionPage />} />
      <Route path="workbenches" element={<WorkbenchesPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
