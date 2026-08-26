import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { ArrangedPage } from "../pages/ArrangedPage";
import type { PageManifest } from "../pages/manifest";
import { NotFoundPage } from "../pages/NotFoundPage";
import { MeLayout, type MeChrome } from "./MeLayout";
import {
  ME_ACCOUNT_PAGE,
  ME_ACCOUNT_PAGE_ID,
  ME_SECURITY_PAGE,
  ME_SECURITY_PAGE_ID,
  ME_SESSIONS_PAGE,
  ME_SESSIONS_PAGE_ID,
  ME_SETTINGS_PAGE,
  ME_SETTINGS_PAGE_ID,
} from "./manifests";
import { useMe } from "./useMe";

// The Me tabs (epic memql#4661, task memql#4674).
//
// Four ARRANGEMENTS now, one per tab, rather than four React bodies inside a
// shared frame. Each is a manifest whose body is a registered widget; the tab
// bar and the heading come from MeLayout through ArrangedPage's own slots,
// because a manifest is data and does not know whose page this is.
//
// What the convergence buys here is not cosmetic. Every tab has a version
// strip and a regenerate control, so somebody who wants their sessions above
// their preferences can have that, stored per-person like every other page
// override -- on the page the epic singled out as the one nobody would expect
// to converge.
export function MeRoutes(): ReactNode {
  const me = useMe();

  const tab = (manifest: PageManifest, pageId: string) => (chrome: MeChrome) => (
    <ArrangedPage
      manifest={manifest}
      pageId={pageId}
      selectedRowId=""
      onSelect={() => {}}
      title={chrome.title}
      blurb={chrome.blurb}
      actions={chrome.actions}
      nav={chrome.nav}
    />
  );

  return (
    <MeLayout account={me.account} loading={me.loading}>
      {(chrome) => (
        <Routes>
          <Route index element={tab(ME_ACCOUNT_PAGE, ME_ACCOUNT_PAGE_ID)(chrome)} />
          <Route path="settings" element={tab(ME_SETTINGS_PAGE, ME_SETTINGS_PAGE_ID)(chrome)} />
          <Route path="sessions" element={tab(ME_SESSIONS_PAGE, ME_SESSIONS_PAGE_ID)(chrome)} />
          <Route path="security" element={tab(ME_SECURITY_PAGE, ME_SECURITY_PAGE_ID)(chrome)} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      )}
    </MeLayout>
  );
}
