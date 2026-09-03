import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app/App";
import { applyStoredTheme } from "./app/theme";
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

const container = document.getElementById("root");
if (container === null) {
  throw new Error("MemQL OS: no #root element in the document");
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
