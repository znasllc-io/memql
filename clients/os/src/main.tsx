import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app/App";
import { applyStoredTheme } from "./app/theme";
import "./styles/index.css";

applyStoredTheme();

const container = document.getElementById("root");
if (container === null) {
  throw new Error("MemQL OS: no #root element in the document");
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
