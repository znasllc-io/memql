import { useCallback, useMemo, useState, type ReactNode } from "react";
import { Route } from "lucide-react";

import { AddButton } from "../../../kit/AddButton";
import { ContentSkeleton, InlineSkeleton } from "../../../kit/ContentSkeleton";
import { InfoDetail } from "../../../kit/InfoDetail";
import { Chip, EmptyState, Notice, RecordList, RecordListSkeleton, RecordRow, RefreshButton } from "../../../kit";
import type { TaskPolicy } from "../taskPolicies";
import { NewRouteWizard } from "./NewRouteWizard";
import { RouteComposer, type RouteDraftState } from "./RouteComposer";
import { routeEntries } from "./routes";
import { GlyphStrip } from "./SourceGlyph";
import { readSource, routeStatus, servingIndex, type RoutingFacts } from "./sources";
import { routeTitle } from "./vocabulary";

// Fleet > Routing > Routes: every route, shipped and yours, as a list -- the
// name, its chain as a strip of source glyphs, and whether it serves right now
// in a few words. A row opens the route's own page, the composer; the Add
// control opens the New route wizard.
//
// DRAFTS LIVE HERE, not on the page: a person who opens a route, changes it,
// goes back to the list and opens it again finds their change, and the row
// says "Unsaved" in the meantime. They are not written to browser storage --
// closing the window ends them, as every Fleet draft does.

type Page = { kind: "list" } | { kind: "route"; name: string } | { kind: "new" };

/** Shipped routes in the order a person meets them; yours after, by name. */
const SHIPPED_ORDER = ["localFirst", "fastLocalFirst", "localOnly", "federationStrongest", "embeddingsBinding"];

export function orderRoutes(routes: readonly TaskPolicy[]): TaskPolicy[] {
  const rank = (r: TaskPolicy) => (r.shipped ? SHIPPED_ORDER.indexOf(r.name) : -1);
  return [...routes].sort((a, b) => {
    if (!!a.shipped !== !!b.shipped) return a.shipped ? -1 : 1;
    if (a.shipped) return (rank(a) === -1 ? 99 : rank(a)) - (rank(b) === -1 ? 99 : rank(b)) || a.name.localeCompare(b.name);
    return routeTitle(a.name).localeCompare(routeTitle(b.name));
  });
}

export function RoutesTab({
  header,
  catalog,
  facts,
  reload,
  onAddMachine,
}: {
  header: (actions: ReactNode, meta?: ReactNode) => ReactNode;
  catalog: { policies: TaskPolicy[]; loading: boolean; error: string };
  facts: RoutingFacts;
  reload: () => void;
  onAddMachine?: () => void;
}) {
  const [page, setPage] = useState<Page>({ kind: "list" });
  const [drafts, setDrafts] = useState<Record<string, RouteDraftState>>({});
  const routes = useMemo(() => orderRoutes(catalog.policies), [catalog.policies]);

  const toList = useCallback(() => setPage({ kind: "list" }), []);

  if (page.kind === "new") {
    return (
      <NewRouteWizard
        routes={routes}
        facts={facts}
        onCancel={toList}
        onCreated={(name) => {
          reload();
          setPage({ kind: "route", name });
        }}
        onAddMachine={onAddMachine}
      />
    );
  }

  if (page.kind === "route") {
    const route = routes.find((r) => r.name === page.name);
    if (route === undefined) {
      // A route just created is not in the list until the re-read lands.
      return catalog.loading ? (
        <div className="os-deploy-scroll"><ContentSkeleton kind="detail" label="Loading the route" /></div>
      ) : (
        <MissingRoute onBack={toList} />
      );
    }
    return (
      <RoutePage
        route={route}
        facts={facts}
        draft={drafts[route.name]}
        setDrafts={setDrafts}
        onBack={toList}
        onChanged={reload}
        onAddMachine={onAddMachine}
      />
    );
  }

  const count = !catalog.loading && !catalog.error ? routes.length : undefined;
  return (
    <div className="os-deploy-scroll">
      {header(
        <>
          <InfoDetail title="Routes">
            <p>A route is a list of sources, tried in order until one can serve. Rules decide which route a call takes.</p>
            <p>A change to a route applies to every rule that takes it. Shipped routes can be changed and restored.</p>
          </InfoDetail>
          <RefreshButton label="Read the routes again" busy={catalog.loading} onClick={reload} />
          {!catalog.error && (!catalog.loading || routes.length > 0) ? <AddButton label="New route" onClick={() => setPage({ kind: "new" })} /> : null}
        </>,
        count,
      )}
      {catalog.error ? <Notice tone="error" sentence="The routes could not be read." detail={catalog.error} /> : null}
      {catalog.loading && routes.length === 0 ? (
        <RecordListSkeleton label="Loading the routes" rows={4} />
      ) : routes.length === 0 && !catalog.error ? (
        <EmptyState title="No routes yet">Add one to choose which sources a call tries, in order.</EmptyState>
      ) : (
        <RecordList as="ul" label="Routes" className="fleet-routes">
          {routes.map((route) => {
            const entries = routeEntries(route);
            const status = routeStatus(entries, facts);
            const title = routeTitle(route.name);
            const fact = drafts[route.name] ? "Unsaved" : route.customized ? "Changed" : route.shipped ? "Shipped" : "";
            return (
              <RecordRow
                key={route.name}
                icon={<Route size={18} aria-hidden />}
                name={title}
                label={`Open ${title}, ${status.word || "checking"}`}
                state={status.word === "" ? <InlineSkeleton label="Checking" /> : status.word}
                // NEUTRAL, ready or not: the accent belongs to the circuit and
                // its "Serves now" (design brief, section 7). The strip's
                // serving glyph carries it here; the words say the rest.
                tone="muted"
                current={status.ready}
                stateExtra={fact ? <Chip tone="muted">{fact}</Chip> : undefined}
                onOpen={() => setPage({ kind: "route", name: route.name })}
              >
                <GlyphStrip readings={entries.map((entry) => readSource(entry, facts))} servingIndex={servingIndex(entries, facts).index} />
              </RecordRow>
            );
          })}
        </RecordList>
      )}
    </div>
  );
}

function RoutePage({
  route,
  facts,
  draft,
  setDrafts,
  onBack,
  onChanged,
  onAddMachine,
}: {
  route: TaskPolicy;
  facts: RoutingFacts;
  draft: RouteDraftState | undefined;
  setDrafts: (update: (held: Record<string, RouteDraftState>) => Record<string, RouteDraftState>) => void;
  onBack: () => void;
  onChanged: () => void;
  onAddMachine?: () => void;
}) {
  const name = route.name;
  const onDraft = useCallback(
    (next: RouteDraftState | undefined) =>
      setDrafts((held) => {
        if (next === undefined) {
          if (!(name in held)) return held;
          const { [name]: _dropped, ...rest } = held;
          return rest;
        }
        return { ...held, [name]: next };
      }),
    [name, setDrafts],
  );
  return <RouteComposer route={route} facts={facts} draft={draft} onDraft={onDraft} onBack={onBack} onChanged={onChanged} onAddMachine={onAddMachine} />;
}

function MissingRoute({ onBack }: { onBack: () => void }) {
  return (
    <div className="os-deploy-scroll">
      <EmptyState title="That route is gone" action={<button type="button" className="fleet-reading-link" onClick={onBack}>Back to Routes</button>}>
        It may have been removed since this page opened.
      </EmptyState>
    </div>
  );
}
