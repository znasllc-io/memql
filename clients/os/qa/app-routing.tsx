import { createRoot } from "react-dom/client";
import { MachineApps } from "../src/apps/fleet/machines/MachineApps";
import { machineFromRow } from "../src/apps/fleet/rows";
import { withSession, machineRow, builtinReply } from "../test/fleet/harness";
import { installQaConnection } from "./connectionShim";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
// Synthetic settings only; no connection to a cluster or model.
installQaConnection({ query: { async fleetSetAppRouting(settings: Record<string, unknown>) {
  if (params.get("state") === "pending") return new Promise(() => {});
  if (params.get("state") === "error") throw new Error("Fixture save refused");
  return builtinReply("fleetSetAppRouting", [{ ...settings, revision: "saved" }]);
} } });
const machine = machineFromRow(machineRow({ id: "machine", ownerUserId: "me", apps: [
  { id: "claude-code", version: "2.1.304", signedIn: true, allowed: true },
  { id: "codex", version: "0.162.1", signedIn: true, allowed: true },
] }));
createRoot(document.getElementById("root")!).render(withSession(<main style={{ maxWidth: params.has("narrow") ? 360 : 880, padding: 20, margin: "20px auto", background: "var(--os-glass-solid)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)" }}><h1>Local machine · Apps</h1><MachineApps machine={machine} now={new Date()} /></main>, { userId: params.has("readonly") ? "other" : "me" }));
