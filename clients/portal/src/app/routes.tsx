import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { AppShell } from "./AppShell";
import { RequireAuth } from "./RequireAuth";
import { AuthCallbackPage } from "../pages/AuthCallbackPage";
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
// The router's basename comes from Vite's `base` (see App), so these paths are
// written WITHOUT the /portal prefix -- one place knows the mount point and it
// is the build config.
//
// TWO TIERS, and the split is load-bearing:
//
//   * auth/callback sits OUTSIDE RequireAuth. It is the one route that must
//     render while signed out -- it is what turns a signed-out session into a
//     signed-in one, so gating it behind a sign-in would be a loop.
//   * Everything else is behind RequireAuth, which renders the sign-in view
//     IN PLACE rather than redirecting. That keeps the requested URL in the
//     address bar, so a deep link survives the round trip without the router
//     having to reconstruct it (auth/pending.ts carries it across the
//     navigation to identity, which destroys the document).
//
// RequireAuth is a rendering decision, NOT an authorization gate -- the real
// enforcement is server-side, per stream and per row. Its own header says so
// at length.

export function AppRoutes(): ReactNode {
  return (
    <Routes>
      <Route path="auth/callback" element={<AuthCallbackPage />} />

      <Route element={<RequireAuth />}>
        <Route element={<AppShell />}>
          {/* The portal has no dashboard yet; concepts is the landing surface. */}
          <Route index element={<Navigate to="/concepts" replace />} />
          <Route path="concepts" element={<ConceptsPage />} />
          <Route path="concepts/:conceptId" element={<ConceptRowsPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Route>
    </Routes>
  );
}
