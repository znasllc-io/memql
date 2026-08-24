import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { DataOriginsPage } from "./DataOriginsPage";

// Mounted from the route table as a splat (`data-origins/*`), like the other
// owner/admin consoles. Nothing here is an authorization gate -- the engine
// refuses every read and every action below cluster owner; these routes
// decide what to OFFER, which is a courtesy.
export function DataOriginsRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<DataOriginsPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
