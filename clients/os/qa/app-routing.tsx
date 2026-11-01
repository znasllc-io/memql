import { createRoot } from "react-dom/client";
import { MachineApps } from "../src/apps/fleet/machines/MachineApps";
import { machineFromRow } from "../src/apps/fleet/rows";
import { withSession, machineRow, builtinReply, rowsResult } from "../test/fleet/harness";
import { installQaConnection } from "./connectionShim";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
// Synthetic settings only; no connection to a cluster or model.
// The sign-in states, by ?signin=. Synthetic rows; nothing here reaches a machine.
const now = Date.now();
const signInRow = (over: Record<string, unknown>) => ({ id: "v1:worker:appSignIn:qa", ownerUserId: "me", workerId: "machine", status: "waiting", url: "", userCode: "", needsCode: false, errorMessage: "", requestedAt: new Date(now).toISOString(), expiresAt: new Date(now + 600000).toISOString(), endedAt: "", ...over });
const signIns: Record<string, Record<string, unknown>[]> = {
  device: [signInRow({ app: "codex", url: "https://auth.openai.com/codex/device", userCode: "ABCD-EFGH" })],
  paste: [signInRow({ app: "claude-code", url: "https://claude.com/cai/oauth/authorize?code=true", needsCode: true })],
  starting: [signInRow({ app: "claude-code", status: "requested" })],
  failed: [signInRow({ app: "claude-code", status: "failed", errorMessage: "Login failed: Request failed with status code 400" })],
  checking: [signInRow({ app: "claude-code", status: "succeeded", endedAt: new Date(now).toISOString() })],
};
const scene = params.get("signin") ?? "";
installQaConnection({ query: { async fleetSetAppRouting(settings: Record<string, unknown>) {
  if (params.get("state") === "pending") return new Promise(() => {});
  if (params.get("state") === "error") throw new Error("Fixture save refused");
  return builtinReply("fleetSetAppRouting", [{ ...settings, revision: "saved" }]);
}, async appSignInsForWorker() { return rowsResult(signIns[scene] ?? []); },
   async fleetAppSignIn() { throw new Error("Laptop is not connected to the cluster right now. Wake it, or check its Cockpit is running, and try again."); },
   async fleetAppSignInCode() { return builtinReply("fleetAppSignInCode", []); },
   async fleetAppSignInCancel() { return builtinReply("fleetAppSignInCancel", []); },
} });
const checkedAt = new Date(now - 4 * 60000).toISOString();
const signedOut = { id: "claude-code", version: "2.1.304", signedIn: false, allowed: true, subscription: "present", readiness: "signed_out", readinessCheckedAt: checkedAt };
const outOfUsage = { id: "codex", version: "0.162.1", signedIn: true, allowed: true, subscription: "present", readiness: "out_of_usage", readinessCheckedAt: checkedAt, usageResetsAt: "2026-11-04T06:52:03Z" };
const ready = { id: "codex", version: "0.162.1", signedIn: true, allowed: true, subscription: "present", readiness: "ready", readinessCheckedAt: checkedAt };
const fixtures: Record<string, Record<string, unknown>[]> = {
  states: [signedOut, outOfUsage],
  unchecked: [{ ...ready, readiness: "unknown", readinessDetail: "Codex is signed in with an API key, which has no plan limit to read, so this machine cannot say whether it can answer" }, { id: "claude-code", version: "2.1.304", signedIn: true, allowed: true, subscription: "present", readiness: "unknown", readinessDetail: "not checked yet" }],
  older: [{ id: "codex", version: "0.162.1", signedIn: true, allowed: true, subscription: "present" }, { id: "claude-code", version: "2.1.304", signedIn: true, allowed: true, subscription: "present" }],
  starting: [signedOut, ready], paste: [signedOut, ready], failed: [signedOut, ready], checking: [signedOut, ready],
  device: [{ ...signedOut, id: "codex", version: "0.162.1" }, { ...ready, id: "claude-code", version: "2.1.304" }],
};
const machine = machineFromRow(machineRow({ id: "machine", ownerUserId: "me", apps: fixtures[scene] ?? [
  { id: "claude-code", version: "2.1.304", signedIn: true, allowed: true },
  { id: "codex", version: "0.162.1", signedIn: true, allowed: true },
], appDescriptors: [{ id: "claude-code", harness: "claude-headless", signIn: true }, { id: "codex", harness: "codex-app-server", signIn: true }] }));
createRoot(document.getElementById("root")!).render(withSession(<main style={{ maxWidth: params.has("narrow") ? 360 : 880, padding: 20, margin: "20px auto", background: "var(--os-glass-solid)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)" }}><h1>Local machine · Apps</h1><MachineApps machine={machine} now={new Date()} /></main>, { userId: params.has("readonly") ? "other" : "me" }));
