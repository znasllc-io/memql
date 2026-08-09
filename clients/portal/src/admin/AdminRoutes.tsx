import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { KeysPage } from "./KeysPage";
import { OverviewPage } from "./OverviewPage";
import { SettingsPage } from "./SettingsPage";
import { TokensPage } from "./TokensPage";

// The absorbed /admin/* surfaces (memql#3324).
//
// The server-rendered templ console carried seven screens. Three of them --
// people, audit, deployments -- are predefined views already (memql#3319), so
// what lands here is the remainder: the overview, sessions and tokens, the
// signing keys, and the cluster settings.
//
// Mounted from the route table as a SPLAT (`admin/*`), so these arrive as
// sub-routes of this module without touching the shared table again. Adding a
// fifth surface is a row in urls.ts plus a Route here.
//
// ===========================================================================
// NOTHING IN THIS MODULE IS AN AUTHORIZATION GATE
// ===========================================================================
// Every one of these surfaces is owner/admin-gated SERVER-side and audited.
// What the pages do with the caller's role is hide controls that would come
// back refused, which is a courtesy to the operator and not enforcement: the
// reads run named queries that carry `requiresOwnerOrAdmin` in their own
// filter, evaluated in the engine against the auth envelope, so a reader who
// reaches these routes gets empty results whatever this code renders. The
// reasoning, and the four capabilities that have no client-reachable seam at
// all, are in src/admin/wire.ts.
export function AdminRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<OverviewPage />} />
      <Route path="tokens" element={<TokensPage />} />
      <Route path="keys" element={<KeysPage />} />
      <Route path="settings" element={<SettingsPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
