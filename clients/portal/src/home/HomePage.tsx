import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { clusterLabelFor } from "../cluster/endpoint";
import { useCluster } from "../cluster/ClusterProvider";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { Band, Container, DataText, PageHeader, Skeleton } from "../ui";
import { payloadOf, useConceptTile, type HomeTileState } from "./useHomeTiles";

// The console (memql#4182, memql#4263): "is my cluster healthy and what
// changed", answerable in five seconds. It is a ROUTER, not a
// dashboard-for-its-own-sake -- every tile is a door into its full surface,
// the numbers are the display-face moment (the one place Squada One carries
// data), and there is no charting here beyond what the surfaces themselves
// already render.
//
// The tiles are one per POPULATION the rail offers -- people, agents,
// customers, sites -- plus the two that TICK: deployments and the audit trail.
// Customers was missing while being one of the five predefined views, which is
// exactly the kind of gap a console exists to close.
//
// Deploy facts (engine version, sync) come from the deploy console and are
// admin/owner reads; below that role the header simply states the host and
// the tiles stand on the caller's own per-row-authz counts. Nothing here
// fakes a number it could not load: a count that filled its single page
// renders with a trailing plus.
export function HomePage(): ReactNode {
  const cluster = clusterLabelFor(globalThis.location);
  const { status } = useCluster();
  const deploy = useDeployConsole();

  const people = useConceptTile("v1:identity:user", false, 0);
  const agents = useConceptTile("v1:agents:agent", false, 0);
  const customers = useConceptTile("v1:identity:account", false, 0);
  const sites = useConceptTile("v1:platform:site", false, 0);
  const deployments = useConceptTile("v1:cluster:deployment", true, 3);
  // The audit trail is the tile that TICKS: security-relevant events land
  // here live, which is the reason an operator glances at a console at all.
  const audit = useConceptTile("v1:identity:auditEvent", true, 5);

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          title="Console"
          blurb={
            <>
              <DataText kind="id">{cluster || "this cluster"}</DataText>
              {deploy.status ? (
                <>
                  {" "}
                  · engine <DataText kind="id">{deploy.status.engineVersion || deploy.status.version || "unknown"}</DataText>
                  {deploy.status.argocd.syncStatus ? ` · ${deploy.status.argocd.syncStatus}` : ""}
                  {deploy.status.argocd.healthStatus ? ` · ${deploy.status.argocd.healthStatus}` : ""}
                </>
              ) : null}
              {status !== "connected" ? " · stream not connected" : ""}
            </>
          }
        />

        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
          <NumberTile label="people" to="/views/people" tile={people} />
          <NumberTile label="agents" to="/views/agents" tile={agents} />
          <NumberTile label="customers" to="/views/customers" tile={customers} />
          <NumberTile label="sites" to="/sites" tile={sites} />
          <NumberTile label="deployments" to="/views/deployments" tile={deployments} />
          <NumberTile label="audit events" to="/views/audit" tile={audit} live />
        </div>

        <Band title="Recent deployments" meta="live">
          <RecentList
            tile={deployments}
            to="/views/deployments"
            renderRow={(row) => {
              const p = payloadOf(row);
              return (
                <>
                  <DataText kind="id">{String(p["engineVersion"] ?? p["deploymentId"] ?? "")}</DataText>
                  <span className="text-xs text-muted">{String(p["status"] ?? "")}</span>
                  <DataText kind="time">{String((row as { createdAt?: unknown }).createdAt ?? "")}</DataText>
                </>
              );
            }}
            empty="No deployments recorded yet."
          />
        </Band>

        <Band title="Recent audit events" meta="live">
          <RecentList
            tile={audit}
            to="/views/audit"
            renderRow={(row) => {
              const p = payloadOf(row);
              return (
                <>
                  <span className="text-sm text-fg">{String(p["action"] ?? p["category"] ?? "event")}</span>
                  <span className="min-w-0 flex-1 truncate text-xs text-muted">
                    {String(p["outcome"] ?? "")}
                  </span>
                  <DataText kind="time">{String((row as { createdAt?: unknown }).createdAt ?? "")}</DataText>
                </>
              );
            }}
            empty="No audit events visible to you yet."
          />
        </Band>
      </section>
    </Container>
  );
}

// The big-number moment. font-display/text-display is Squada One, reserved
// for the wordmark and exactly this surface (ui/README type scale).
//
// The number is EXACT -- the engine counts server-side under the caller's own
// authz. A tile that could not read renders an em dash and its reason: nothing
// here fabricates a number, and "0" would be a claim this surface is not
// entitled to make.
function NumberTile({
  label,
  to,
  tile,
  live = false,
}: {
  label: string;
  to: string;
  tile: HomeTileState;
  live?: boolean;
}): ReactNode {
  return (
    <Link
      to={to}
      className="group rounded-lg border border-line bg-surface p-4 hover:border-line-strong hover:bg-raised"
    >
      {tile.loading ? (
        <Skeleton variant="stat" />
      ) : (
        <>
          <div className="font-display text-display leading-none text-fg">
            {tile.error !== "" ? "—" : tile.count}
          </div>
          <div className="mt-1 flex items-center gap-2 text-xs text-muted">
            <span className="uppercase tracking-wide">{label}</span>
            {live ? <span className="text-subtle">live</span> : null}
          </div>
          {tile.error !== "" ? (
            <p className="mt-1 text-xs text-subtle">could not read: {tile.error}</p>
          ) : null}
        </>
      )}
    </Link>
  );
}

function RecentList({
  tile,
  to,
  renderRow,
  empty,
}: {
  tile: HomeTileState;
  to: string;
  renderRow: (row: HomeTileState["rows"][number]) => ReactNode;
  empty: string;
}): ReactNode {
  if (tile.loading) return <Skeleton variant="rows" rows={3} />;
  if (tile.error !== "") {
    return <p className="text-sm text-muted">Could not read this: {tile.error}</p>;
  }
  if (tile.rows.length === 0) {
    return <p className="text-sm text-muted">{empty}</p>;
  }
  return (
    <ul className="flex flex-col gap-1.5">
      {tile.rows.map((row) => (
        <li key={String((row as { id?: unknown }).id)}>
          <Link
            to={to}
            className="flex flex-wrap items-baseline gap-x-3 gap-y-1 rounded border border-line bg-surface px-3 py-1.5 hover:bg-raised"
          >
            {renderRow(row)}
          </Link>
        </li>
      ))}
    </ul>
  );
}
