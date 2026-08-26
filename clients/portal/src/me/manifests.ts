import type { PageManifest } from "../pages/manifest";

// The Me tabs, as manifests (epic memql#4661, task memql#4674).
//
// ===========================================================================
// THE EXEMPLAR NON-DATA PAGE
// ===========================================================================
// The epic's own directive: "we're not gonna have a custom page for one thing
// -- even the profile page". This is that page, and it is the hardest case for
// the claim, because almost nothing on it is a population: it is one row
// (yours), a set of credentials, and a list of your own sessions.
//
// It converges anyway, and the shape of the convergence is the interesting
// part. Each tab is a manifest whose body is a registered widget, the tab bar
// is route-level navigation in ArrangedPage's `nav` slot, and the heading --
// your own name -- is a title override, because a manifest is data and does
// not know who is reading.
//
// What that buys is not cosmetic: every tab now has a version strip and a
// regenerate control, so somebody who wants their sessions above their
// preferences can have that, and it is stored per-person like every other
// page override.
//
// SESSIONS IS THE ONE REAL POPULATION here, and it is the one section over a
// concept rather than a widget-only page.

const USER = "v1:identity:user";
const SESSION = "v1:identity:authSession";

export const ME_ACCOUNT_PAGE_ID = "me.account";
export const ME_SETTINGS_PAGE_ID = "me.settings";
export const ME_SESSIONS_PAGE_ID = "me.sessions";
export const ME_SECURITY_PAGE_ID = "me.security";

function widgetPage(pageId: string, title: string, blurb: string, widgetId: string): PageManifest {
  return {
    pageId,
    title,
    blurb,
    sections: [
      {
        conceptId: USER,
        arrangement: {
          conceptId: USER,
          elements: [{ element: "widget", band: "roll", options: { widgetId } }],
        },
        // A tab without its own contents is not a rearrangement of the tab.
        required: [{ element: "widget", band: "roll", options: { widgetId } }],
      },
    ],
  };
}

export const ME_ACCOUNT_PAGE = widgetPage(
  ME_ACCOUNT_PAGE_ID,
  "Account",
  "Who this cluster knows you as.",
  "meAccount",
);

export const ME_SETTINGS_PAGE = widgetPage(
  ME_SETTINGS_PAGE_ID,
  "Settings",
  "Your own settings on this cluster.",
  "profilePreferences",
);

export const ME_SECURITY_PAGE = widgetPage(
  ME_SECURITY_PAGE_ID,
  "Security",
  "How you sign in, and the devices that can.",
  "meSecurity",
);

export const ME_SESSIONS_PAGE: PageManifest = {
  pageId: ME_SESSIONS_PAGE_ID,
  title: "Sessions",
  blurb: "Everywhere you are signed in right now.",
  sections: [
    {
      conceptId: SESSION,
      arrangement: {
        conceptId: SESSION,
        elements: [
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          { element: "widget", band: "roll", options: { widgetId: "meSessions" } },
        ],
      },
      required: [{ element: "widget", band: "roll", options: { widgetId: "meSessions" } }],
    },
  ],
};
