import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app/App";
import { applyStoredTheme } from "./app/theme";
import { captureConceptOpen } from "./apps/concepts/openConcept";
import { captureConnectReturn } from "./apps/deployables/sources/connectReturn";
import "./styles/index.css";

applyStoredTheme();

// The GitHub-connect return, read and scrubbed BEFORE anything renders (epic
// memql#4915). Module scope is what makes it strictly earlier than
// AuthProvider's own read of the same query string, so neither can eat the
// other's parameters -- this one removes only its own two and leaves the
// path, the hash and everything else alone. The value waits in that module
// until the Shell exists to receive it; a browser that arrived here with no
// marker parks nothing and this is a no-op.
captureConnectReturn(window);

// A concept named in the address, read and scrubbed at the same moment and
// for the same reasons (epic memql#5009). This is how the VS Code
// extension hands a concept over to the console: MemQL OS has no router, so
// the equivalent of the portal's `/concepts/:id` route is a parameter turned
// into an open intent. Each reader removes only its own parameter, so the
// two cannot eat each other's.
captureConceptOpen(window);

const container = document.getElementById("root");
if (container === null) {
  throw new Error("MemQL OS: no #root element in the document");
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
