import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { StoreDetailPage } from "./StoreDetailPage";
import { StoresPage } from "./StoresPage";

// The Shopify connector's operator surface (memql#4398). Mounted as a SPLAT
// (`stores/*`) so a change here needs no edit to the shared route table --
// the convention SitesRoutes and IntegrationsRoutes already use.
//
// TWO ADDRESSES:
//
//   /stores           every configured store, with its health, plus the
//                     add-a-store form
//   /stores/:storeId  one store: scopes, subscriptions, per-domain sync
//                     state, and the backfill / reconcile / pause actions
//
// The detail screen is a child address rather than a modal, for the reason
// the whole portal is held to (#3316): a store somebody is about to back-fill
// is a link they can send a colleague and a page that survives a refresh.
export function StoresRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<StoresPage />} />
      <Route path=":storeId" element={<StoreDetailPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
