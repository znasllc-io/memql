import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { CampaignEditorPage } from "./CampaignEditorPage";
import { CampaignsPage } from "./CampaignsPage";
import { IntegrationsPage } from "./IntegrationsPage";

// Integration management (memql#3323) owns everything under /integrations.
//
// Mounted from the route table as a SPLAT (`integrations/*`), so this module
// adds its own sub-routes -- an integration's detail, a campaign's editor --
// without editing the shared route table again.
//
// THREE ADDRESSES:
//
//   /integrations                        the registry: what is wired up, and
//                                        whether each thing is configured and
//                                        actually working
//   /integrations/campaigns              audiences, templates, campaigns
//   /integrations/campaigns/:campaignId  one campaign's editor
//                                        (`new` creates)
//
// The editor is a CHILD ADDRESS rather than a modal, for the reason the whole
// portal is held to (#3316): a campaign somebody is drafting is a link they
// can send to a colleague and a page that survives a refresh. A modal is none
// of those, and retrofitting the URL later means lifting and serializing state
// that should have been an address from the start.

export function IntegrationsRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<IntegrationsPage />} />
      <Route path="campaigns" element={<CampaignsPage />} />
      <Route path="campaigns/:campaignId" element={<CampaignEditorPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
