import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { ModuleDetailPage } from "./ModuleDetailPage";
import { ModulesPage } from "./ModulesPage";

// Mounted from the route table as a splat (`modules/*`), like the other
// owner/admin consoles. Nothing here is an authorization gate -- the engine
// refuses module reads below owner/admin and the flip below owner
// (memql#4188); these routes decide what to OFFER, which is a courtesy.
export function ModulesRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<ModulesPage />} />
      <Route path=":kind/:name" element={<ModuleDetailPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
