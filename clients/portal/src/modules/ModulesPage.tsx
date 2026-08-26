import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Module } from "@znasllc-io/memql-sdk-core/client";

import {
  Badge,
  Band,
  Button,
  Container,
  DataText,
  ErrorNotice,
  Skeleton,
} from "../ui";
import { AreaFrame } from "../app/AreaFrame";
import { useAdminAccess } from "../admin/useAdminConsole";
import { ModulesRefused } from "./ModulesRefused";
import { groupByKind, KIND_BLURBS, KIND_LABELS, stateTone, useModulesInventory } from "./useModules";
import { modulePath } from "./urls";

// The Modules browser (memql#4191): what this cluster RUNS, grouped by the
// locked kinds -- packs, integrations, node types, components. "Module" is
// the collective term, never a fourth kind
// (docs/public/concepts/modules.md).
//
// PER-NODE VS CLUSTER HONESTY is part of the wire contract (memql#4188):
// every row says which truth tier its state is ("cluster" = the shared
// graph; "node" = the answering binary's own registries), and the header
// names the node that answered. The surface renders what the engine said
// and adds nothing.
export function ModulesPage(): ReactNode {
  const { role, canAdminister, resolved } = useAdminAccess();
  const state = useModulesInventory(canAdminister);

  if (!canAdminister) {
    return <ModulesRefused role={role} resolved={resolved} />;
  }

  const inventory = state.inventory;
  return (
    <Container>
      <AreaFrame
        area="concepts"
        pageId="concepts.modules"
        subtitle="Concepts"
        title="Modules"
        blurb="Everything this cluster runs, by kind, with the switch each kind actually has. Packs flip per instance; integrations follow their configuration; node types scale; components are the engine itself."
        actions={
          <Button size="xs" onClick={state.reload} busy={state.loading} busyLabel="Reading…">
            Refresh
          </Button>
        }
        meta={
          inventory ? (
            <span className="text-xs text-subtle">
              answered by <DataText kind="id">{inventory.reportingNodeId || "unknown"}</DataText>
              {inventory.reportingNodeType ? ` (${inventory.reportingNodeType})` : ""}
            </span>
          ) : undefined
        }
      >

        {state.error !== "" ? (
          <ErrorNotice sentence="Could not read this cluster's modules." detail={state.error} />
        ) : inventory === null ? (
          <Skeleton variant="rows" rows={8} />
        ) : (
          groupByKind(inventory.modules).map((group) => (
            <Band
              key={group.kind}
              title={KIND_LABELS[group.kind] ?? group.kind}
              meta={KIND_BLURBS[group.kind]}
              panel
            >
              <ul className="divide-y divide-line">
                {group.rows.map((row) => (
                  <ModuleRow key={`${row.kind}:${row.name}`} row={row} />
                ))}
              </ul>
            </Band>
          ))
        )}
      </AreaFrame>
    </Container>
  );
}

function ModuleRow({ row }: { row: Module }): ReactNode {
  return (
    <li>
      <Link
        to={modulePath(row.kind, row.name)}
        className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 hover:bg-raised"
      >
        <span className="min-w-32 text-sm font-medium text-fg">{row.name}</span>
        <Badge tone={stateTone(row.state)}>{row.state || "unknown"}</Badge>
        <span
          className="text-xs text-subtle"
          title={
            row.scope === "cluster"
              ? "Cluster-wide fact, read from the shared graph"
              : "This node's own fact -- another binary may differ"
          }
        >
          {row.scope || "node"}
        </span>
        <span className="min-w-0 flex-1 truncate text-xs text-muted">
          {row.stateDetail || row.description}
        </span>
      </Link>
    </li>
  );
}
