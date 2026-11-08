import type { ReactNode } from "react";
import { vi } from "vitest";
import { ConceptPage } from "../src/apps/concepts/ConceptPage";
import { DEFAULT_CONCEPTS_SETTINGS } from "../src/apps/concepts/settings";
import { conceptOf, fakeConnection, nodeOf, withSession } from "../test/concepts/harness";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";
const concept = conceptOf({ id: "v1:accounts:clientAccount", description: "Organizations you work with.", displayCard: { primary: "name", secondary: "description" }, fields: [
  { name: "name", kind: "string", required: true, enumValues: [], description: "The organization's name." },
  { name: "description", kind: "string", required: false, enumValues: [], description: "What the organization does." },
] });
const rows = Array.from({ length: 25 }, (_, n) => nodeOf(`v1:accounts:clientAccount:company-${n}`, {
  name: n === 24 ? "Ancient needle archive" : `Organization ${String(n + 1).padStart(2, "0")}`,
  description: "A client account with its original record fields.",
}, concept.id));
export const CONCEPT_PAGED_VIEWS = {
  "concepts-paged": {
    connect: () => {
      const connection = fakeConnection({ pages: [
      { rows: rows.slice(0, 10), cursor: "next" },
      { rows: rows.slice(10, 20), cursor: "last" },
      { rows: rows.slice(20), cursor: "" },
    ], searchPages: [{ rows: [rows[24]!], cursor: "" }] });
      // Exercise the real disconnected adapter both before any answer and
      // after a page is inspected; only this browser fixture loses transport.
      const transport = new URLSearchParams(window.location.search).get("transport");
      if (transport === "initial" || transport === "page") {
        const read = connection.query.executeNamed;
        let reads = 0;
        connection.query.executeNamed = vi.fn(async (...args: unknown[]) => {
          if (transport === "initial" || reads++ > 0) throw Object.assign(new Error("stream closed"), { reason: "transport" });
          return read(...args);
        });
      }
      return connection;
    },
    wrap: (children: ReactNode) => withSession(<OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{ cols: 8, rows: 5 }}><div className="os-window-content" style={{ height: "100dvh" }}>{children}</div></OsProvider>),
    render: () => <ConceptPage conceptId={concept.id} concept={concept} registryState="live" settings={DEFAULT_CONCEPTS_SETTINGS} onBack={() => {}} />,
  },
};
