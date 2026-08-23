import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { AppSessionPage } from "./AppSessionPage";
import { MachinesPage } from "./MachinesPage";
import { SESSION_ROUTE_PATTERN } from "./urls";

// The machines surface (memql#4363), mounted from the route table as a SPLAT
// (`machines/*`) the way every other feature directory is.
//
// TWO ADDRESSES:
//
//   /machines                     your machines, their apps, the delegation
//                                 policy, and the runs that resulted
//   /machines/sessions/:sessionId one run's live transcript
//
// The transcript is a CHILD ADDRESS rather than a modal, the standard the
// whole portal is held to (#3316): a run somebody wants a colleague to look
// at is a link, and a page that survives a refresh.
export function MachinesRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<MachinesPage />} />
      <Route path={SESSION_ROUTE_PATTERN} element={<AppSessionPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
