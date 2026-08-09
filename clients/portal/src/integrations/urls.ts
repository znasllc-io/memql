// Addresses inside the integrations surface (memql#3323).
//
// Every destination is a URL, not a piece of component state -- the standard
// the whole portal is held to (#3316). A campaign an operator is editing is a
// link they can send to a colleague and a page that survives a refresh.
//
// Paths are written WITHOUT the /portal prefix: the router's basename comes
// from Vite's `base`, so one place knows the mount point and it is the build
// config.

export const INTEGRATIONS_ROOT = "/integrations";

export function integrationsPath(): string {
  return INTEGRATIONS_ROOT;
}

export function campaignsPath(): string {
  return `${INTEGRATIONS_ROOT}/campaigns`;
}

// campaignPath addresses one campaign's editor. `new` is a reserved slug
// rather than a query parameter so the create form and the edit form are the
// same route with the same component, which is what keeps them from drifting
// into two implementations of one screen.
export function campaignPath(campaignId: string): string {
  return `${INTEGRATIONS_ROOT}/campaigns/${encodeURIComponent(campaignId)}`;
}

export const NEW_CAMPAIGN_SLUG = "new";

export function newCampaignPath(): string {
  return campaignPath(NEW_CAMPAIGN_SLUG);
}
