import { createRoot } from "react-dom/client";
import { useRef } from "react";
import type { ConceptRegistryDelta } from "@znasllc-io/memql-sdk-core/client";

import { AccountsApp } from "../src/apps/accounts/AccountsApp";
import { LocalAccountsSettingsStore } from "../src/apps/accounts/settings";
import { SELF_ACCOUNT_ID } from "../src/apps/accounts/rows";
import { PageNavigationProvider } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";
import { setEffectiveCapabilities } from "../src/system/roles";
import { applyPackStyles } from "../src/themes/registry";
import { accountRow, fakeConnection, rowsResult, withSession } from "../test/accounts/harness";
import { seededAccessFor } from "../test/seededAccess";
import { installQaConnection } from "./connectionShim";
import "../src/styles/index.css";

// Local fixtures only: every answer is in memory. No cluster, credentials,
// production data, or external request is used. "Move" changes no real row.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
setEffectiveCapabilities(seededAccessFor("owner"));
globalThis.fetch = async () => Response.json({ error: "This local fixture does not contact services." }, { status: 403 });

const concepts = [["reviews", "review"], ["fylo", "applicationDetail"], ["identity", "user"]].map(([domain, entity]) => ({
  id: `v1:${domain}:${entity}`, version: "v1", domain: domain!, entity: entity!, description: "", type: "concept",
}));
const conn = fakeConnection({
  clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, name: "Our Studio" }), accountRow({ id: "client-example", name: "Example Client" })],
  accountInventory: new Error("The inventory could not finish in time."),
  accountAssignmentStatus: [{ kind: "switch", enabled: true, held: false }],
});
// The browser's vi shim records calls but does not implement test-runner
// mock replacement methods. Wrap the original function directly instead.
const original = conn.query.executeNamed;
Object.assign(conn.query, { executeNamed: async (name: string, call: string) => {
  if (name === "accountUnplaced") return rowsResult(params.get("populated") === "true" ? [
    { kind: "unplaced", concept: "v1:reviews:review", namespace: "reviews", name: "review", rows: 2 },
  ] : []);
  if (name === "accountRows") {
    if (call.includes("v1:identity:user")) throw new Error("This kind does not support accounts.");
    const concept = call.includes("v1:fylo:applicationDetail") ? "v1:fylo:applicationDetail" : "v1:reviews:review";
    return rowsResult([
      ...(params.get("rows") === "empty" ? [] : [{ kind: "row", id: "fixture-row-1", concept, primary: "Example unassigned record", secondary: "Local fixture", followsParent: false }]),
      { kind: "page", nextCursor: "" },
    ]);
  }
  if (name === "accountRowsMove") return rowsResult([{
    kind: call.includes("preview: true") ? "wouldMove" : "moved",
    concept: call.includes("v1:fylo:applicationDetail") ? "v1:fylo:applicationDetail" : "v1:reviews:review",
    rows: 1,
  }]);
  return original(name, call);
} });
let registryReads = 0;
Object.assign(conn.query, {
  subscribeConceptRegistry: (onDelta: (delta: ConceptRegistryDelta) => void, options: { onError: (error: Error) => void }) => {
    registryReads += 1;
    if (params.get("registry") === "failed" && registryReads === 1) options.onError(new Error("The list of kinds is temporarily unavailable."));
    else if (params.get("registry") !== "pending") onDelta({ generation: 1, reset: true, added: concepts, removed: [] });
    return { unsubscribe: () => {} };
  },
});
installQaConnection(conn);
const memory = new Map<string, string>();
const settings = new LocalAccountsSettingsStore({ getItem: key => memory.get(key) ?? null, setItem: (key, value) => { memory.set(key, value); } });

function Fixture() {
  const content = useRef<HTMLDivElement>(null);
  return (
    <div className="os-window" role="dialog" aria-label="Accounts" style={{ position: "fixed", inset: 0, maxWidth: params.get("width") === "narrow" ? 390 : undefined, display: "flex", flexDirection: "column" }}>
      <div className="os-window-body">
        <PageNavigationProvider root={content} trail={[]}>
          <TrailRow fallback="Accounts" />
          <div ref={content} className="os-window-content" data-os-window-content>
            <AccountsApp sectionId="accounts" navigate={() => {}} askContext={() => {}} store={settings} />
          </div>
        </PageNavigationProvider>
      </div>
    </div>
  );
}
createRoot(document.getElementById("root")!).render(withSession(<Fixture />));
