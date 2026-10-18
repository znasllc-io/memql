import { useCallback, useEffect, useMemo, useState } from "react";
import { Link2 } from "lucide-react";

import { AddButton } from "../../../kit/AddButton";
import { ContentSkeleton, InlineSkeleton } from "../../../kit/ContentSkeleton";
import { PageHelp } from "../../../kit/pageHelp";
import { Chip, EmptyState, Head, Notice, RecordList, RecordListSkeleton, RecordRow, AutoRefresh } from "../../../kit";
import type { RulesState } from "../../settings/rulesFacts";
import type { TaskPolicy } from "../taskPolicies";
import { NewChainWizard } from "./NewChainWizard";
import { RouteComposer, type RouteDraftState } from "./RouteComposer";
import { routeEntries } from "./routes";
import { GlyphStrip } from "./SourceGlyph";
import { readSource, routeStatus, servingIndex, type RoutingFacts } from "./sources";
import { routeTitle } from "./vocabulary";

// Fleet > Routing > Rules > Chains: every chain the rules can take, shipped
// and yours, as a list -- the name, its sources as a strip of glyphs, and
// whether it serves right now in a few words. A row opens the chain's own
// page, the composer; the Add control opens the New chain wizard.
//
// A CHAIN IS WHAT A RULE TAKES. It was the Routes tab until routes became the
// thing a person picks in Ask (Auto, Local, Apps and their own, design record
// 2026-10-18-routes-design.md); the engine still calls these policies, and the
// rules still name them, so they live one step under Rules, unchanged.
//
// DRAFTS LIVE HERE, not on the page: a person who opens a chain, changes it,
// goes back to the list and opens it again finds their change, and the row
// says "Unsaved" in the meantime. They are not written to browser storage --
// closing the window ends them, as every Fleet draft does.
//
// A CHAIN JUST CREATED IS PENDING, not missing. The wizard's save and the
// page it opens land in one render, before the re-read that brings the chain
// back has even started -- so "That chain is gone" would paint for a frame,
// and stay if the re-read failed. The page waits for the NEXT settled read
// (`catalog.settled`) and says what that read said.

type Page = { kind: "list" } | { kind: "route"; name: string } | { kind: "new" };

/** Shipped chains in the order a person meets them; yours after, by name. */
const SHIPPED_ORDER = ["localFirst", "fastLocalFirst", "localOnly", "federationStrongest", "embeddingsBinding"];

export function orderRoutes(routes: readonly TaskPolicy[]): TaskPolicy[] {
  const rank = (r: TaskPolicy) => (r.shipped ? SHIPPED_ORDER.indexOf(r.name) : -1);
  return [...routes].sort((a, b) => {
    if (!!a.shipped !== !!b.shipped) return a.shipped ? -1 : 1;
    if (a.shipped) return (rank(a) === -1 ? 99 : rank(a)) - (rank(b) === -1 ? 99 : rank(b)) || a.name.localeCompare(b.name);
    return routeTitle(a.name).localeCompare(routeTitle(b.name));
  });
}

export function ChainsTab({
  onBack,
  catalog,
  facts,
  rules,
  reload,
  request,
  onOpenRules,
  onAddMachine,
}: {
  /** Back to the Rules list this page was opened from. */
  onBack: () => void;
  catalog: { policies: TaskPolicy[]; loading: boolean; error: string; settled: number };
  facts: RoutingFacts;
  /** The rules, read once for the section: how many take each route. */
  rules?: Pick<RulesState, "rules" | "read">;
  /** Re-read the routes AND the rules; they share one engine revision. */
  reload: () => void;
  /** A rule asking for a chain's page ("Change Fast local first"). */
  request?: { name: string; n: number } | null;
  /** Open Rules on the rules that take this chain. */
  onOpenRules?: (route: string) => void;
  onAddMachine?: () => void;
}) {
  const [page, setPage] = useState<Page>({ kind: "list" });
  const [drafts, setDrafts] = useState<Record<string, RouteDraftState>>({});
  const [pending, setPending] = useState<{ name: string; after: number } | null>(null);
  const routes = useMemo(() => orderRoutes(catalog.policies), [catalog.policies]);

  const toList = useCallback(() => setPage({ kind: "list" }), []);

  useEffect(() => {
    if (request) setPage({ kind: "route", name: request.name });
  }, [request?.n]);

  if (page.kind === "new") {
    return (
      <NewChainWizard
        routes={routes}
        facts={facts}
        onCancel={toList}
        onCreated={(name) => {
          setPending({ name, after: catalog.settled });
          reload();
          setPage({ kind: "route", name });
        }}
        onRefused={reload}
        onAddMachine={onAddMachine}
      />
    );
  }

  if (page.kind === "route") {
    const route = routes.find((r) => r.name === page.name);
    if (route === undefined) {
      const waiting = catalog.loading || (pending !== null && pending.name === page.name && catalog.settled <= pending.after);
      if (waiting) return <div className="os-deploy-scroll"><ContentSkeleton kind="detail" label="Loading the chain" /></div>;
      if (catalog.error) {
        return (
          <div className="os-deploy-scroll">
            <Notice tone="warn" sentence="The chain could not be read." detail={catalog.error} />
            <button type="button" className="fleet-reading-link" onClick={reload}>Read the chains again</button>
            <button type="button" className="fleet-reading-link" onClick={toList}>Back to the list</button>
          </div>
        );
      }
      return <MissingRoute onBack={toList} />;
    }
    // The LAST KNOWN count through a re-read: the rules are read again after
    // every write, and a line that vanished and came back would move the
    // composer under the cursor at the moment of saving.
    const known = rules !== undefined && (rules.read || rules.rules.length > 0);
    const taking = known ? rules.rules.filter((r) => r.policy === route.name).length : null;
    return (
      <RoutePage
        route={route}
        facts={facts}
        draft={drafts[route.name]}
        setDrafts={setDrafts}
        rulesTaking={taking}
        onBack={toList}
        onChanged={reload}
        onOpenRules={onOpenRules ? () => onOpenRules(route.name) : undefined}
        onAddMachine={onAddMachine}
      />
    );
  }

  const count = !catalog.loading && !catalog.error ? routes.length : undefined;
  return (
    <div className="os-deploy-scroll">
      <div className="fleet-routing-header">
        <Head title="Chains" meta={count} breadcrumbs={[{ label: "Rules", onSelect: onBack }, { label: "Chains" }]} back={{ label: "Rules", onSelect: onBack }}>
          <PageHelp title="Chains">
            <p>A chain is a list of sources, tried in order until one can serve. Rules decide which chain a call takes when its route is Auto.</p>
            <p>A change to a chain applies to every rule that takes it. Shipped chains can be changed and restored.</p>
          </PageHelp>
          <AutoRefresh busy={catalog.loading} onRefresh={reload} />
          {!catalog.error && (!catalog.loading || routes.length > 0) ? <AddButton label="New chain" onClick={() => setPage({ kind: "new" })} /> : null}
        </Head>
      </div>
      {catalog.error ? <Notice tone="error" sentence="The chains could not be read." detail={catalog.error} /> : null}
      {catalog.loading && routes.length === 0 ? (
        <RecordListSkeleton label="Loading the chains" rows={4} />
      ) : routes.length === 0 && !catalog.error ? (
        <EmptyState title="No chains yet">Add one to choose which sources a call tries, in order.</EmptyState>
      ) : (
        <RecordList as="ul" label="Chains" className="fleet-routes">
          {routes.map((route) => {
            const entries = routeEntries(route);
            const status = routeStatus(entries, facts);
            const title = routeTitle(route.name);
            const fact = drafts[route.name] ? "Unsaved" : route.customized ? "Changed" : route.shipped ? "Shipped" : "";
            return (
              <RecordRow
                key={route.name}
                icon={<Link2 size={18} aria-hidden />}
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
  rulesTaking,
  onBack,
  onChanged,
  onOpenRules,
  onAddMachine,
}: {
  route: TaskPolicy;
  facts: RoutingFacts;
  draft: RouteDraftState | undefined;
  setDrafts: (update: (held: Record<string, RouteDraftState>) => Record<string, RouteDraftState>) => void;
  rulesTaking: number | null;
  onBack: () => void;
  onChanged: () => void;
  onOpenRules?: () => void;
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
  return (
    <RouteComposer
      route={route}
      facts={facts}
      draft={draft}
      onDraft={onDraft}
      rulesTaking={rulesTaking}
      onBack={onBack}
      onChanged={onChanged}
      onOpenRules={onOpenRules}
      onAddMachine={onAddMachine}
    />
  );
}

function MissingRoute({ onBack }: { onBack: () => void }) {
  return (
    <div className="os-deploy-scroll">
      <EmptyState title="That chain is gone" action={<button type="button" className="fleet-reading-link" onClick={onBack}>Back to Chains</button>}>
        It may have been removed since this page opened.
      </EmptyState>
    </div>
  );
}
