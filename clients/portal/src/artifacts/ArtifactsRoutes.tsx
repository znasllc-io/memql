import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { ArtifactDetailPage } from "./ArtifactDetailPage";
import { ArtifactsPage } from "./ArtifactsPage";

// The Library, browsed and labelled (task 4 of the artifacts-labels epic).
// Mounted from the route table as a SPLAT (`artifacts/*`), the same
// convention SitesRoutes uses and for the same reason (routes.tsx): a change
// here does not need an edit to the shared route table.
//
// TWO ADDRESSES:
//
//   /artifacts               every artifact the caller owns, plus the label
//                             filter (?label=) and the create form
//   /artifacts/:artifactId   one artifact's detail + label editor
//
// The detail screen is a CHILD ADDRESS rather than a modal -- the standard
// the whole portal is held to (#3316): an artifact someone is labelling is a
// link they can send a colleague, and a page that survives a refresh.
export function ArtifactsRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<ArtifactsPage />} />
      <Route path=":artifactId" element={<ArtifactDetailPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
