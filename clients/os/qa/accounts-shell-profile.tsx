import { applyReducedMotionFixture } from "./reducedMotion";
import { createRoot } from "react-dom/client";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { Shell } from "../src/chrome/Shell";
import { AuthProvider } from "../src/auth/AuthProvider";
import { LocalDesktopStore } from "../src/system/store";
import { applyPackStyles } from "../src/themes/registry";
import { setEffectiveCapabilities } from "../src/system/roles";
import { seededAccessFor } from "../test/seededAccess";
import { SEEDED_LADDER } from "../test/seededLadder";
import { StubAskTransport } from "../test/ask/stubTransport";
import { installQaConnection } from "./connectionShim";
import { profileScenes } from "./profileScenes";
import "../src/styles/index.css";

// Local fixtures only. All HTTP requests are intercepted here; no service,
// credentials, permission writes, account switch or provider action is involved.
const params = new URLSearchParams(location.search);
const role = params.get("role") === "viewer" ? "viewer" : "owner";
const capabilities = seededAccessFor(role);
if (params.get("profile") === "denied") {
  // Keep the older personal doors: an explicit Profile deny must still win.
  for (const entry of capabilities) {
    if (entry.verb === "read" && entry.resource === "app:profile") entry.effect = "deny";
  }
}
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
if (params.get("motion") === "reduced") applyReducedMotionFixture();
setEffectiveCapabilities(capabilities);
const config = { identityUrl: location.origin, identityApiBaseUrl: location.origin, oauthClientId: "fixture", authEnabled: false, domain: "memql.example.test" };
const nativePages = {
  "/me/devices": { Layout: { Title: "Passkeys and sessions" }, PasskeysEnabled: true, Passkeys: [{ ID: "fixture-passkey", Label: "This Mac", Active: true, Authenticator: "Platform authenticator", Backup: "Backed up", CreatedAt: "2026-11-01", LastUsedAt: "2026-11-13" }], SessionsAvailable: true, Sessions: [{ ID: "fixture-session", Label: "This browser", Origin: location.origin, ThisDevice: true, FirstSeen: "2026-11-13", LastSeen: "Now", Expires: "2026-11-14" }] },
  "/me/tokens": { Layout: { Title: "Access tokens" }, Tokens: [{ ID: "fixture-token", Label: "Local script", Active: true, LastUsedAt: "2026-11-12" }], NextCursor: "" },
  "/me/settings": { Layout: { Title: "Settings" }, SignInSecurity: { Available: true, PasskeyOnly: false, PasskeyCountKnown: true, ActivePasskeys: 1, SharedMailbox: false }, LegalAcceptance: { Available: true, Items: [] } },
};
globalThis.fetch = async (input, init) => {
  const url = new URL(String(input), location.origin);
  if (url.origin !== location.origin || (init?.method && init.method !== "GET")) return Response.json({ error: "This fixture does not perform changes." }, { status: 403 });
  if (url.pathname.endsWith("/runtime-config.json")) return Response.json(config);
  const data = nativePages[url.pathname as keyof typeof nativePages];
  return data ? Response.json({ page: url.pathname.slice(1), data, csrf: "local-fixture" }) : Response.json({ error: "No fixture for this request." }, { status: 404 });
};
const base = profileScenes(({ children }) => <>{children}</>)[params.get("empty") === "true" ? "profile-quiet" : "profile"]!.connect();
const empty = () => Promise.resolve(new Result({ data: [] } as never));
const query = new Proxy({ ...base.query,
  activeRoles: async () => new Result({ data: SEEDED_LADDER.map(rung => ({ ...rung, active: true })) } as never),
  effectiveCapabilitiesForActor: async () => new Result({ data: [{ ok: true, entries: capabilities }] } as never),
  executeNamed: (name: string, call: string) => name === "myBudget" ? base.query.executeNamed(name, call) : empty(),
}, { get: (target, name) => name in target ? target[name as keyof typeof target] : empty });
installQaConnection({ ...base, query, dispatcher: null,
  onStatusChange: (callback: (event: { status: "connected" }) => void) => {
    callback({ status: "connected" });
    return () => {};
  },
});
const memory = new Map<string, string>();
createRoot(document.getElementById("root")!).render(<AuthProvider><Shell
  layout={params.get("layout") === "phone" ? "phone" : "desktop"}
  onSignOut={() => { document.getElementById("root")!.textContent = "Signed out of the local fixture"; }}
  access={{ userId: "v1:identity:user:ada", primaryEmail: "ada@example.test", role, roleName: role === "owner" ? "Owner" : "Viewer", rank: role === "owner" ? 400 : 50 }}
  config={config} ports={{ disableConnection: true, askTransport: new StubAskTransport(), askVoice: null,
    store: new LocalDesktopStore({ getItem: key => memory.get(key) ?? null, setItem: (key, value) => { memory.set(key, value); } }) }}
/></AuthProvider>);
