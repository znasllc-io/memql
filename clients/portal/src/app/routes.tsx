import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { AppShell } from "./AppShell";
import { RequireAuth } from "./RequireAuth";
import { AuthCallbackPage } from "../pages/AuthCallbackPage";
import { ConceptPage } from "../pages/ConceptPage";
import { ConceptRowsPane } from "../pages/ConceptRowsPane";
import { ConceptSchemaPane } from "../pages/ConceptSchemaPane";
import { ConceptsPage } from "../pages/ConceptsPage";
import { NotFoundPage } from "../pages/NotFoundPage";
import {
  CONCEPTS_ROUTE_PATTERN,
  CONCEPT_ROUTE_PATTERN,
  CONCEPT_ROW_CHILD_PATTERN,
  CONCEPT_SCHEMA_CHILD_PATTERN,
  conceptsPath,
} from "../concepts/urls";

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
//
// THE CONCEPT BROWSER'S SHAPE (memql#3316). Three addresses, nested so the
// middle one stays mounted across the third:
//
//   /concepts                        the registry: search + domain filter
//   /concepts/:conceptId             one concept -- its rows
//   /concepts/:conceptId/rows/:rowId ...with that row's detail open
//   /concepts/:conceptId/schema      ...its declared fields instead
//
// The row route is a CHILD of the concept route rather than a sibling, and
// that is deliberate: ConceptPage owns the keyset walk, so opening a row
// re-renders the pane without restarting paging. Sibling routes would remount
// the whole workspace and re-fetch page one on every click.
//
// Concept ids contain colons; src/concepts/urls.ts owns the encoding that
// keeps them readable in an address bar and exact through a round trip.

export function AppRoutes(): ReactNode {
  return (
    <Routes>
      <Route path="auth/callback" element={<AuthCallbackPage />} />

      <Route element={<RequireAuth />}>
        <Route element={<AppShell />}>
          {/* The portal has no dashboard yet; concepts is the landing surface. */}
          <Route index element={<Navigate to={conceptsPath()} replace />} />
          <Route path={CONCEPTS_ROUTE_PATTERN} element={<ConceptsPage />} />
          <Route path={CONCEPT_ROUTE_PATTERN} element={<ConceptPage />}>
            <Route index element={<ConceptRowsPane />} />
            <Route path={CONCEPT_ROW_CHILD_PATTERN} element={<ConceptRowsPane />} />
            <Route path={CONCEPT_SCHEMA_CHILD_PATTERN} element={<ConceptSchemaPane />} />
          </Route>
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Route>
    </Routes>
  );
}
