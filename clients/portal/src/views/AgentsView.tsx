import type { ReactNode } from "react";
import {
  KANBAN_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  STAT_TILE_ELEMENT,
  TABLE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { useViewRows } from "../cluster/useViewRows";
import { ViewElement } from "./ViewElement";
import { Band, type ViewProps } from "./ViewLayout";

// Agents: what this cluster runs, and what people have let it do.
//
// LAYOUT RATIONALE. Two populations that answer each other. The fleet is a
// TABLE -- an operator compares agents against each other (which role, whose,
// still active) and a grid is what comparison wants. The grants are a BOARD
// grouped by the action they authorize, because a grant is not compared
// against other grants, it is READ BY KIND: "what may these agents do without
// asking?" is a question whose answer is a set of columns.
//
// CAPABILITIES ARE THE DETAIL PANE, deliberately. An agent's capabilities are
// a nested object (avatar, vision, claw, skill ids, provider config), and a
// nested object flattened into table cells is unreadable. view-kit's Record
// element already renders the wire's nesting exactly as the concept browser
// does, so selecting an agent is how you read them -- one renderer, not a
// second one invented here.

// The standing grants. Read is caller-scoped by the engine
// (@rowAuthz(owner="userId")), which is why the band says whose they are.
const AUTHORIZATION_CONCEPT_ID = "v1:agents:agentAuthorization";

export function AgentsView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  const grants = useViewRows(AUTHORIZATION_CONCEPT_ID);
  const selection = selectedRowId ? { selectedRowId } : {};

  return (
    <>
      <Band>
        <ViewElement
          element={STAT_TILE_ELEMENT}
          rows={rows}
          concept={concept}
          // colorIndex is the only top-level number on an agent, and its
          // total is a palette index nobody wants summed.
          options={{ bindings: { metric: [] } }}
        />
      </Band>

      <Band title="By kind" meta="assistant answers people; specialist answers tools">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ bindings: { category: "kind", value: [] } }}
        />
      </Band>

      <Band title="The fleet" meta="Select an agent to read its capabilities" panel>
        <ViewElement
          element={TABLE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ ...selection, sort: { field: "name", direction: "asc" } }}
          onSelect={onSelect}
        />
      </Band>

      <Band
        title="Standing authorizations"
        meta={
          grants.concept === undefined
            ? "not published by this node"
            : "Grants you made — the cluster scopes this read to your own"
        }
      >
        {grants.concept === undefined ? (
          <p className="text-sm text-subtle">
            This node does not publish the agent-authorization concept, so grants
            cannot be listed here.
          </p>
        ) : (
          <ViewElement
            element={KANBAN_ELEMENT}
            rows={grants.rows}
            concept={grants.concept}
            // Columns are what the grant permits. The card says which agent
            // and under which plan kind; the rest is one click away.
            options={{ bindings: { group: "action", label: "agentId", detail: "planKind" } }}
          />
        )}
      </Band>
    </>
  );
}
