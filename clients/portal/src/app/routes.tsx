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
import { ViewsGalleryPage } from "../pages/ViewsGalleryPage";
import { ViewPage } from "../views/ViewPage";
import { AdminRoutes } from "../admin/AdminRoutes";
import { StoresRoutes } from "../stores/StoresRoutes";
import { ArtifactsRoutes } from "../artifacts/ArtifactsRoutes";
import { ComposeRoutes } from "../compose/ComposeRoutes";
import { DataOriginsRoutes } from "../dataorigins/DataOriginsRoutes";
import { DeployablesRoutes, RetiredSitesRedirect } from "../deployables/DeployablesRoutes";
import { FleetRoutes } from "../fleet/FleetRoutes";
import { IntegrationsRoutes } from "../integrations/IntegrationsRoutes";
import { MeRoutes } from "../me/MeRoutes";
import { ModulesRoutes } from "../modules/ModulesRoutes";
import { NexusRoutes } from "../nexus/NexusRoutes";
import { HomePage } from "../home/HomePage";
import {
  CONCEPTS_ROUTE_PATTERN,
  CONCEPT_ROUTE_PATTERN,
  CONCEPT_ROW_CHILD_PATTERN,
  CONCEPT_SCHEMA_CHILD_PATTERN,
} from "../concepts/urls";
import { RetiredViewRedirect } from "../views/RetiredViewRedirect";
import {
  RETIRED_VIEW_IDS,
  VIEWS_ROUTE_SEGMENT,
  VIEW_ROUTE_PATTERN,
  VIEW_ROW_CHILD_PATTERN,
  viewPath,
} from "../views/urls";
import { DATA_ORIGINS_ROUTE_PATTERN } from "../dataorigins/urls";

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
//
// THE PREDEFINED VIEWS (memql#3319) mirror that shape one altitude up:
//
//   /views/:viewId                   a designed view of one concept
//   /views/:viewId/rows/:rowId       ...with that row's detail open
//
// Same nesting decision, same reason: ViewPage owns the keyset walk, so
// opening a row re-renders the body rather than restarting paging. A view id
// is a slug this repo chooses, so unlike a concept id it needs no encoding;
// the ROW id still does, and goes through the same encodeSegment.
//
// The index still lands on /concepts. The five views are surfaces an operator
// picks deliberately from the nav; the registry is the honest default landing
// for a console that has not been told which cluster it is looking after.
//
// THE REMAINING SURFACES ARE MOUNTED AS SPLATS:
//
//   /integrations/*   integration + campaign management   (memql#3323)
//   /compose/*        the user-composed view builder      (memql#3320)
//   /admin/*          the absorbed server-rendered admin  (memql#3324)
//   /deployables/*    what this cluster hosts: list, create, deploy from the
//                     Library, roll back, enable/disable, delete (memql#4346,
//                     replacing /sites/* from memql#3717)
//   /fleet/*          machines + workbenches -- where work runs (memql#4349)
//   /modules/*        the module inventory + pack enablement (memql#4191)
//   /data-origins/*   what MemQL owns, mirrors and pushes out (memql#4378)
//   /stores/*         the Shopify stores a connector mirrors (memql#4389)
//   /artifacts/*      the Library, browsed and labelled
//   /nexus/*          Nexus: a goal's world in 3D, its constructs, its
//                     replay (memql#4369)
//   /me/*             the signed-in person: account, sessions, security
//
// Each owns a `<name>Routes` module that declares its own sub-routes. Three
// separate changes would otherwise each need an edit here and in AppShell, and
// in one worktree that is three chances to clobber the other two. A splat costs
// one line and means the route table stops being a shared bottleneck.

export function AppRoutes(): ReactNode {
  return (
    <Routes>
      <Route path="auth/callback" element={<AuthCallbackPage />} />

      <Route element={<RequireAuth />}>
        <Route element={<AppShell />}>
          {/* The console home (memql#4182): cluster identity + live tiles,
              every one a door into its full surface. */}
          <Route index element={<HomePage />} />
          {/* The Views gallery is the area's own landing (memql#4655): the
              five built-in views, the caller's composed ones and the door to
              composing another, which were three separate things in the rail
              and one kind of thing to the person reading them. Declared
              BEFORE the :viewId route -- a literal segment and a parameter at
              the same depth are ranked by the router, and stating the order
              here makes the intent visible in the diff. */}
          <Route path={VIEWS_ROUTE_SEGMENT} element={<ViewsGalleryPage />} />
          <Route path={VIEW_ROUTE_PATTERN} element={<ViewPage />} />
          <Route path={`${VIEW_ROUTE_PATTERN}/${VIEW_ROW_CHILD_PATTERN}`} element={<ViewPage />} />
          {/* Renamed in memql#4526: the two directory views took their
              concepts' names, so /views/people and /views/customers point at
              /views/users and /views/accounts. Declared per DEPTH rather than
              as a splat -- RetiredViewRedirect's header has the router-ranking
              reason, and it is the kind of thing that looks right in a diff
              and sends a bookmarked row to "No such view". */}
          {Object.entries(RETIRED_VIEW_IDS).flatMap(([from, to]) => [
            <Route
              key={from}
              path={`${VIEWS_ROUTE_SEGMENT}/${from}`}
              element={<RetiredViewRedirect to={to} />}
            />,
            <Route
              key={`${from}/rows`}
              path={`${VIEWS_ROUTE_SEGMENT}/${from}/${VIEW_ROW_CHILD_PATTERN}`}
              element={<RetiredViewRedirect to={to} />}
            />,
          ])}
          <Route path={CONCEPTS_ROUTE_PATTERN} element={<ConceptsPage />} />
          <Route path={CONCEPT_ROUTE_PATTERN} element={<ConceptPage />}>
            <Route index element={<ConceptRowsPane />} />
            <Route path={CONCEPT_ROW_CHILD_PATTERN} element={<ConceptRowsPane />} />
            <Route path={CONCEPT_SCHEMA_CHILD_PATTERN} element={<ConceptSchemaPane />} />
          </Route>
          <Route path="compose/*" element={<ComposeRoutes />} />
          <Route path="stores/*" element={<StoresRoutes />} />
          <Route path="integrations/*" element={<IntegrationsRoutes />} />
          <Route path="admin/*" element={<AdminRoutes />} />
          <Route path="deployables/*" element={<DeployablesRoutes />} />
          <Route path="fleet/*" element={<FleetRoutes />} />
          <Route path="modules/*" element={<ModulesRoutes />} />
          <Route path={`${DATA_ORIGINS_ROUTE_PATTERN}/*`} element={<DataOriginsRoutes />} />
          <Route path="artifacts/*" element={<ArtifactsRoutes />} />
          <Route path="nexus/*" element={<NexusRoutes />} />
          {/* The person, rather than the cluster: the account this
              connection resolved, its live sessions, and how it can be
              entered. Reached from the rail's profile row. Identity's own
              /me/* self-service pages do NOT move here -- this surface
              renders and links (docs/public/operate/portal.md). */}
          <Route path="me/*" element={<MeRoutes />} />
          {/* Retired in memql#4264: the Deployments view carries the four
              verbs now, with the confirmations this page had and that view's
              Ship band did not. Redirected rather than 404'd -- whoever
              bookmarked it did nothing wrong, and a Not Found would read as
              "the capability is gone" when it moved. */}
          <Route path="cluster-ops" element={<Navigate to={viewPath("deployments")} replace />} />
          {/* Renamed in memql#4346: Sites became Deployables when the concept
              gained an owner and a person -- not only an operator -- could put
              a thing on the internet. Same redirect reasoning as above, and
              the SPLAT carries the tail so a bookmarked /sites/:siteId lands on
              that deployable rather than on the list. */}
          <Route path="sites/*" element={<RetiredSitesRedirect />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Route>
    </Routes>
  );
}
