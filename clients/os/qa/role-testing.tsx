import { RoleTestNotice } from "../src/auth/RoleTestBanner";
import { createRoot } from "react-dom/client";
import { RoleTestingSection } from "../src/apps/users/RoleTestingSection";
import { SessionProvider } from "../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG } from "../src/cluster/config";
import { installQaConnection } from "./connectionShim";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic UI fixture: no credentials, live sessions or network writes.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
let sessions = params.has("empty") ? [] : [{ id: "fixture", userId: "test-persona", role: "reader", expiresAt: String(Math.floor(Date.now()/1000)+1800) }];
installQaConnection({ dispatcher: { async sendAndWait(message: { identityAdmin: { roleTest: { operation: string } } }) {
  if (params.has("pending")) return new Promise(() => {});
  if (params.has("error")) throw new Error("The connection closed. Reopen Role testing to retry.");
  if (message.identityAdmin.roleTest.operation === "end") sessions = [];
  if (message.identityAdmin.roleTest.operation === "start") throw new Error("Starting sessions is unavailable in this fixture.");
  return { identityAdminResult: { ok: true, roleTestSessions: sessions } };
} } });
createRoot(document.getElementById("root")!).render(<SessionProvider value={{ access: null, config: UNKNOWN_RUNTIME_CONFIG }}><><RoleTestNotice role="Viewer" expiresAt={Math.floor(Date.now()/1000)+1800} disabled={false} error="" onEnd={() => {}} /><main style={{ margin: "96px auto", width: "min(100% - 24px, 680px)", padding: 20, background: "var(--os-glass-solid)", border: "1px solid var(--os-line)" }}><RoleTestingSection role={params.get("role") ?? "owner"} /></main></></SessionProvider>);
