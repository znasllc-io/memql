import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { ConstructsPage } from "./ConstructsPage";
import { GoalLayout } from "./GoalLayout";
import { GoalsPage } from "./GoalsPage";
import { MapPage } from "./MapPage";
import { ReplayPage } from "./ReplayPage";

// Nexus, mounted from the route table as a SPLAT (`nexus/*`) -- the same
// convention AdminRoutes, SitesRoutes and ArtifactsRoutes use, and for the
// same reason (routes.tsx): a change here does not need an edit to the shared
// route table, which in a repository worked by several sessions at once is
// three chances to clobber somebody else's line.
//
// SIX ADDRESSES, and the nesting is the decision:
//
//   /nexus                             your goals -- the list, statuses shown
//                                      (was a redirect; GoalsPage's header
//                                      says why that reversed)
//   /nexus/:planId                     the Map
//   /nexus/:planId/node/:nodeId        ...with that node's detail open
//   /nexus/:planId/constructs          what the goal authored
//   /nexus/:planId/replay              how it got here
//   /nexus/:planId/replay/node/:nodeId ...with a node open, moment intact
//
// The three pages are CHILDREN of a `:planId` layout route rather than
// siblings, which is what keeps the goal's world read once across a tab
// click instead of seven reads and seven subscriptions per tab (GoalLayout's
// own header says more). It is the same nesting decision the concept browser
// makes for its row route, for the same reason.
//
// The node routes are children of the PAGE that opens them, not of the goal.
// Replay's event list is the map's keyboard index (design 4.4) and pressing
// Enter there must not throw away the moment the scrubber is parked at, which
// a single node address under the Map would do.

export function NexusRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<GoalsPage />} />
      <Route path=":planId" element={<GoalLayout />}>
        <Route index element={<MapPage />} />
        <Route path="node/:nodeId" element={<MapPage />} />
        <Route path="constructs" element={<ConstructsPage />} />
        <Route path="replay" element={<ReplayPage />} />
        <Route path="replay/node/:nodeId" element={<ReplayPage />} />
      </Route>
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
