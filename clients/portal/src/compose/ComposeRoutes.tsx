import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { ComposeHomePage } from "./ComposeHomePage";
import { ComposedViewPage } from "./ComposedViewPage";
import { ComposerEditPage, ComposerPage } from "./ComposerPage";
import {
  COMPOSED_VIEW_EDIT_PATTERN,
  COMPOSED_VIEW_PATTERN,
  COMPOSE_NEW_PATTERN,
} from "./urls";

// User-composed views (memql#3320) own everything under /compose.
//
// Mounted from the route table as a SPLAT (`compose/*`), so the composer adds
// its own sub-routes without editing the shared route table again.
//
// FOUR ADDRESSES, and the split mirrors the concept browser's:
//
//   /compose                  what you have composed, and what you could
//   /compose/new?concept=...  the composer, over a selection
//   /compose/:viewId          a saved view, read
//   /compose/:viewId/edit     ...reopened in the composer
//
// The SELECTION IS IN THE QUERY STRING and a saved view is a PATH, which is
// the honest distinction: a selection is a list (a repeated parameter is the
// only unambiguous way to carry one), a view is a single durable thing with an
// id somebody will bookmark. Both are URLs rather than component state, for
// the reason the route table gives at length: a destination held in state has
// to be lifted, serialized and parsed later anyway.
//
// The composer route is `new` rather than `:something`, so it cannot be shadowed
// by a view id -- it is declared first and matched exactly.

export function ComposeRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<ComposeHomePage />} />
      <Route path={COMPOSE_NEW_PATTERN} element={<ComposerPage />} />
      <Route path={COMPOSED_VIEW_EDIT_PATTERN} element={<ComposerEditPage />} />
      <Route path={COMPOSED_VIEW_PATTERN} element={<ComposedViewPage />} />
      <Route path="*" element={<ComposeHomePage />} />
    </Routes>
  );
}
