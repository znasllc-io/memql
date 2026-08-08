import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app/App";
import { applyStoredTheme } from "./app/theme";
import "./styles/index.css";

// Theme BEFORE mount, deliberately: applyStoredTheme writes the data-theme
// attribute synchronously, so the first paint is already in the right
// palette. Doing it in an effect would paint one frame of the wrong theme on
// every load for anyone whose choice differs from their OS.
applyStoredTheme();

const container = document.getElementById("root");
if (container === null) {
  // index.html owns the mount point, so this is a build/deploy inconsistency
  // rather than a runtime condition. Fail loudly instead of rendering
  // nothing into a blank page with a clean console.
  throw new Error("memQL portal: no #root element in the document");
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
