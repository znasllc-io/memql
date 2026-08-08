import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { AppShell } from "./AppShell";
import { ConceptRowsPage } from "../pages/ConceptRowsPage";
import { ConceptsPage } from "../pages/ConceptsPage";
import { NotFoundPage } from "../pages/NotFoundPage";

// The route table.
//
// Every destination is a URL, not a piece of component state. That is a hard
// requirement of #3316 (deep-linkable, refresh-survivable views) and it is
// cheaper to establish now than to retrofit: a selection kept in state has to
// be lifted, serialized and parsed later, whereas a route is already all
// three.
//
// The router's basename comes from Vite's `base` (see PortalRouter), so these
// paths are written WITHOUT the /portal prefix -- one place knows the mount
// point and it is the build config.

export function AppRoutes(): ReactNode {
  return (
    <Routes>
      <Route element={<AppShell />}>
        {/* The portal has no dashboard yet; concepts is the landing surface. */}
        <Route index element={<Navigate to="/concepts" replace />} />
        <Route path="concepts" element={<ConceptsPage />} />
        <Route path="concepts/:conceptId" element={<ConceptRowsPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
