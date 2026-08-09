import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { KeysPage } from "./KeysPage";
import { OverviewPage } from "./OverviewPage";
import { PeoplePage } from "./PeoplePage";
import { SettingsPage } from "./SettingsPage";
import { TokensPage } from "./TokensPage";

// The absorbed /admin/* surfaces (memql#3324).
//
// The server-rendered templ console carried seven screens. Audit and
// deployments are predefined views already (memql#3319), and so is the People
// POPULATION -- what lands here is the remainder plus the CHANGE surface for a
// person: the overview, people, sessions and tokens, the signing keys, and the
// cluster settings. (Why an admin People surface exists beside the People
// view: see the note at the top of PeoplePage.tsx.)
//
// Mounted from the route table as a SPLAT (`admin/*`), so these arrive as
// sub-routes of this module without touching the shared table again. Adding a
// surface is a row in urls.ts plus a Route here.
//
// ===========================================================================
// NOTHING IN THIS MODULE IS AN AUTHORIZATION GATE
// ===========================================================================
// Every one of these surfaces is owner/admin-gated SERVER-side and audited.
// What the pages do with the caller's role is hide controls that would come
// back refused, which is a courtesy to the operator and not enforcement:
//
//   READS run named queries carrying `requiresOwnerOrAdmin` in their own
//   filter, evaluated in the engine against the auth envelope, so a reader who
//   reaches these routes gets empty results whatever this code renders.
//
//   WRITES go through IdentityAdminClient onto component/identity/adminops,
//   which refuses below owner/admin with PERMISSION_DENIED and an
//   `admin_auth_forbidden` audit event -- the same refusal, with the same
//   reason, the retired templ route wrote.
//
// The reasoning is in src/admin/wire.ts.
export function AdminRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<OverviewPage />} />
      <Route path="people" element={<PeoplePage />} />
      <Route path="tokens" element={<TokensPage />} />
      <Route path="keys" element={<KeysPage />} />
      <Route path="settings" element={<SettingsPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
