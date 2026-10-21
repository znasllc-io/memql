import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Cpu, Gauge, Route, Terminal } from "lucide-react";

import { AddButton } from "../../../kit/AddButton";
import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { PageHelp } from "../../../kit/pageHelp";
import { AutoRefresh, Chip, EmptyState, Notice, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { AutoPage } from "./AutoPage";
import { jobStatus } from "./JobBoard";
import { NewRouteModeWizard } from "./NewRouteModeWizard";
import { RouteModePage, type RouteModeDraft } from "./RouteModePage";
import { EDITABLE_JOBS, canEditRoute, embedderReading, jobSourcesOf, type EmbedderReading, type RouteMode } from "./routeModes";
import type { RoutingFacts } from "./sources";
import { useAutoBalance, useBoundEmbedder, useMyBudget, useRouteModes, useRouteWrites } from "./useRouteModes";

// Fleet > Routing > Routes: the routes a person picks for their messages in
// Ask (design record 2026-10-18-routes-design.md, "Fleet, Routing, Routes").
//
//   Auto, Local, Apps   the built-ins, cluster-wide; Local and Apps can be
//                       changed by an owner or developer and restored
//   Yours               the person's own routes, with the Add control on the
//                       section heading; private to them
//
// A row opens the route's page: Auto's balance, budget and picks, or a
// route's jobs and the sources that serve them. Each page REPLACES the list
// (DESIGN.md rule 11) with `<- Routes` in the trail.
//
// EVERYONE WHO CAN OPEN FLEET SEES THIS TAB: anyone may make routes of their
// own. What an owner or developer adds is the right to change the built-ins
// and Auto's balance -- the acts appear for them and are absent for anyone
// else (rule 12).
//
// DRAFTS LIVE HERE, not on the page, so a route opened, changed, left and
// opened again keeps the change; its row says "Unsaved" meanwhile.

type Page = { kind: "list" } | { kind: "route"; id: string } | { kind: "new" };

const ICONS: Record<RouteMode["kind"], typeof Gauge> = { auto: Gauge, local: Cpu, apps: Terminal, custom: Route };

/**
 * A route's readiness in a few words. The engine's own answer when it gave
 * one; otherwise read from the same facts the jobs are drawn from, so a
 * cluster that predates routes still says something true.
 */
export function routeReadiness(route: RouteMode, facts: RoutingFacts, embedder: EmbedderReading, fromEngine: boolean): { ready: boolean; word: string } {
  if (fromEngine && (route.ready || route.reason !== "")) return { ready: route.ready, word: route.ready ? "Ready" : route.reason };
  if (embedder.state === "none") return { ready: false, word: "Needs an embedding model" };
  const known = embedder.state === "bound";
  if (route.kind === "auto") return { ready: known, word: known ? "Ready" : "" };
  const jobs = jobSourcesOf(route);
  for (const job of EDITABLE_JOBS) {
    const status = jobStatus(jobs[job], facts);
    if (status.text === "") return { ready: false, word: "" };
    if (!status.ready) return { ready: false, word: status.text };
  }
  return { ready: known, word: known ? "Ready" : "" };
}

export function RoutesTab({
  header,
  facts,
  composes,
  request,
  onAddMachine,
  onOpenModels,
}: {
  header: (actions: ReactNode, meta?: ReactNode) => ReactNode;
  facts: RoutingFacts;
  /** Owner or developer: may change Local, Apps and Auto's balance. */
  composes: boolean;
  /** Another surface asking for a route's page (Ask's "Manage routes"). */
  request?: { id: string; n: number } | null;
  onAddMachine?: () => void;
  onOpenModels?: () => void;
}) {
  const modes = useRouteModes(true);
  const writes = useRouteWrites();
  const embedder = useBoundEmbedder(true);
  const balance = useAutoBalance(true);
  const budget = useMyBudget(true);
  const [page, setPage] = useState<Page>({ kind: "list" });
  const [drafts, setDrafts] = useState<Record<string, RouteModeDraft>>({});
  const [pending, setPending] = useState<string | null>(null);

  const toList = useCallback(() => setPage({ kind: "list" }), []);
  const rereadAll = useCallback(() => {
    modes.reread();
    embedder.reread();
  }, [modes.reread, embedder.reread]);

  useEffect(() => {
    if (request && request.id !== "") setPage({ kind: "route", id: request.id });
  }, [request?.n]);

  // A ROUTE JUST CREATED IS PENDING, not missing, until a read brings it back.
  useEffect(() => {
    if (pending !== null && modes.routes.some((r) => r.id === pending)) setPending(null);
  }, [pending, modes.routes]);

  const embedderName = embedderReading(embedder);
  const personal = modes.routes.filter((r) => r.scope === "personal");
  const builtIns = modes.routes.filter((r) => r.scope === "cluster");

  if (page.kind === "new") {
    return (
      <NewRouteModeWizard
        routes={modes.routes}
        facts={facts}
        embedder={embedderName}
        writes={writes}
        onCancel={toList}
        onCreated={(id) => {
          modes.reread();
          if (id === "") {
            setPage({ kind: "list" });
            return;
          }
          setPending(id);
          setPage({ kind: "route", id });
        }}
        onRefused={modes.reread}
        onAddMachine={onAddMachine}
        onOpenModels={onOpenModels}
      />
    );
  }

  if (page.kind === "route") {
    const route = modes.routes.find((r) => r.id === page.id);
    if (page.id === "auto") {
      return (
        <AutoPage
          route={route}
          facts={facts}
          embedder={embedderName}
          balance={balance}
          budget={budget}
          editable={composes}
          writes={writes}
          onBack={toList}
        />
      );
    }
    if (route === undefined) {
      if (modes.loading || pending === page.id) return <div className="os-deploy-scroll"><ContentSkeleton kind="detail" label="Loading the route" /></div>;
      return (
        <div className="os-deploy-scroll">
          <EmptyState title="That route is gone" action={<button type="button" className="fleet-reading-link" onClick={toList}>Back to Routes</button>}>
            It may have been deleted since this page opened.
          </EmptyState>
        </div>
      );
    }
    return (
      <RouteModePage
        key={route.id}
        route={route}
        facts={facts}
        embedder={embedderName}
        editable={canEditRoute(route, composes)}
        saves={modes.available && writes.supported}
        revision={modes.revision}
        writes={writes}
        takenNames={personal.filter((r) => r.id !== route.id).map((r) => r.name)}
        draft={drafts[route.id]}
        onDraft={(next) =>
          setDrafts((held) => {
            if (next === undefined) {
              if (!(route.id in held)) return held;
              const { [route.id]: _dropped, ...rest } = held;
              return rest;
            }
            return { ...held, [route.id]: next };
          })
        }
        onBack={toList}
        onChanged={rereadAll}
        onDeleted={() => {
          modes.reread();
          setPage({ kind: "list" });
        }}
        onAddMachine={onAddMachine}
        onOpenModels={onOpenModels}
      />
    );
  }

  const row = (route: RouteMode) => {
    const Icon = ICONS[route.kind];
    const readiness = routeReadiness(route, facts, embedderName, modes.available);
    const fact = drafts[route.id] ? "Unsaved" : route.customized ? "Changed" : "";
    return (
      <RecordRow
        key={route.id}
        icon={<Icon size={18} aria-hidden />}
        name={route.name}
        secondary={route.description}
        label={`Open ${route.name}, ${readiness.word || "checking"}`}
        state={readiness.word === "" ? undefined : readiness.word}
        tone="muted"
        current={readiness.ready}
        stateExtra={fact ? <Chip tone="muted">{fact}</Chip> : undefined}
        onOpen={() => setPage({ kind: "route", id: route.id })}
      />
    );
  };

  return (
    <div className="os-deploy-scroll">
      {header(
        <>
          <PageHelp title="Routes">
            <p>A route decides which source serves each job: quick tasks, main work, deep reasoning, and search and memory. You pick a route for each message in Ask.</p>
            <p>Auto chooses for every call. Local keeps everything on local models; Apps uses Claude Code or Codex. Routes you make are yours alone. Search and memory always uses the cluster's embedding model.</p>
          </PageHelp>
          <AutoRefresh busy={modes.loading} onRefresh={rereadAll} />
        </>,
      )}
      {modes.error ? <Notice tone="error" sentence="The routes could not be read." detail={modes.error} /> : null}
      {!modes.available && !modes.loading ? (
        <Notice tone="warn" sentence="This cluster does not save routes yet." next="The shipped routes are shown as they are." />
      ) : null}
      {modes.loading && modes.routes.length === 0 ? (
        <RecordListSkeleton label="Loading the routes" rows={3} />
      ) : (
        <>
          <RecordList as="ul" label="Built-in routes" className="fleet-routes fleet-route-modes">
            {builtIns.map(row)}
          </RecordList>
          {modes.available ? (
            <div className="fleet-route-yours">
              <Subhead
                meta={personal.length > 0 ? personal.length : undefined}
                actions={writes.supported ? <AddButton label="New route" onClick={() => setPage({ kind: "new" })} /> : null}
              >
                Yours
              </Subhead>
              {personal.length === 0 ? (
                <p className="os-caption fleet-route-yours-empty">Routes you make appear here.</p>
              ) : (
                <RecordList as="ul" label="Your routes" className="fleet-routes fleet-route-modes">
                  {personal.map(row)}
                </RecordList>
              )}
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}
