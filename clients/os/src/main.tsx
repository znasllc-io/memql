import { captureShopifyInstallation } from "./modules/connections/shopifyInstallation";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app/App";
import { DesktopOnly, isPhoneOrTablet } from "./chrome/DesktopOnly";
import { applyStoredTheme } from "./app/theme";
import { applyRememberedThemePack } from "./themes/bootHint";
import { captureConceptOpen } from "./apps/concepts/openConcept";
import { captureRunOpen } from "./apps/deployables/pipelines/openRun";
import { captureShopifyReturn } from "./apps/deployables/store/connectReturn";
import { captureConnectReturn } from "./apps/deployables/sources/connectReturn";
import "./styles/index.css";

applyStoredTheme();
// The last theme this browser showed, from the first frame (themes/bootHint.ts).
applyRememberedThemePack();

// Phones and tablets get the "coming soon" screen and nothing else (the rule
// is in chrome/DesktopOnly.tsx). Decided before the captures below, so the
// address stays whole for "Copy this link", and instead of App, so sign-in
// never starts.
const desktopOnly = isPhoneOrTablet(navigator, window.matchMedia?.bind(window));

if (!desktopOnly) {
  // The GitHub-connect return, read and scrubbed BEFORE anything renders (epic
  // memql#4915). Module scope is what makes it strictly earlier than
  // AuthProvider's own read of the same query string, so neither can eat the
  // other's parameters -- this one removes only its own two and leaves the
  // path, the hash and everything else alone. The value waits in that module
  // until the Shell exists to receive it; a browser that arrived here with no
  // marker parks nothing and this is a no-op.
  captureShopifyInstallation(window);
  captureConnectReturn(window);
  captureShopifyReturn(window);

  // A concept named in the address, read and scrubbed at the same moment and
  // for the same reasons (epic memql#5009). This is how the VS Code
  // extension hands a concept over to the console: MemQL OS has no router, so
  // the equivalent of the portal's `/concepts/:id` route is a parameter turned
  // into an open intent. Each reader removes only its own parameter, so the
  // two cannot eat each other's.
  captureConceptOpen(window);

  // A pipeline run named in the address (epic memql#5479): a GitHub check run's
  // details link, `?pipelineRun=<run id>`, opens the run page. Read and scrubbed
  // here for the concept link's reasons, removing only its own parameter.
  captureRunOpen(window);
}

const container = document.getElementById("root");
if (container === null) {
  throw new Error("MemQL OS: no #root element in the document");
}

createRoot(container).render(
  <StrictMode>
    {desktopOnly ? <DesktopOnly /> : <App />}
  </StrictMode>,
);
