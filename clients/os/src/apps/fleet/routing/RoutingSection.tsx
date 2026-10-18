import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import "./routing.css";

import { AutoRefresh, Head } from "../../../kit";
import { LocalTabs } from "../../../kit/LocalTabs";
import { useSession } from "../../../chrome/access";
import { accessAdmits, type OsAppProps } from "../../../system/registry";
import { useRules } from "../../settings/rulesFacts";
import { useTaskPolicies } from "../taskPolicies";
import { DECISIONS_SECTION_RESOURCE, RULES_SECTION_RESOURCE } from "./access";
import { ChainsTab } from "./ChainsTab";
import { HistoryTab } from "./HistoryTab";
import { MachinesTab } from "./MachinesTab";
import { routesLike } from "./routes";
import { RoutesTab } from "./RoutesTab";
import { RulesTab } from "./RulesTab";
import { useFleetCapacity } from "./useRouteModes";
import { useRoutingFacts } from "./useRoutingFacts";

// Fleet > Routing (design brief 2026-09-28, sections 2-5; routes, 2026-10-18):
// ONE home for how a model call finds something to answer it.
//
//   Routes     what a person picks for their messages in Ask -- Auto, Local,
//              Apps and their own -- each job served by its sources in order
//   Rules      "when work looks like X, take chain Y", for calls on Auto, in
//              the order tried; one step under it, Chains: the ordered lists
//              of sources the rules name, composed on a chain's own page
//   Machines   which of your machines takes a call sent to them
//   History    what actually served recent calls, and why
//
// THE GATES. Routes is everyone's: anyone may make routes of their own, and
// the built-ins' acts are present only for an owner or developer. Rules and
// Chains sit behind `app:settings/rules` (owner or developer) and History
// behind `app:settings/decisions` (owner, developer, admin) -- the
// capabilities that gated them in Settings, so nobody's reach changed. A tab a
// person cannot open is absent, never a tab that refuses. Machines is per
// person and ungated.
//
// CAPACITY IS READ HERE, ONCE, for every tab: whether a machine can take a
// call right now (`fleetCapacity`) changes with no graph event, so the
// section's header carries the AutoRefresh that re-reads it while visible.
//
// VISITED TABS STAY MOUNTED, hidden, so a draft route or a half-written rule
// survives a look at History. Each tab draws the section's Head on its list
// page and its own Head on a drill-down, so there is one Head on screen.
//
// ONE REVISION, SO ONE RE-READ. The engine keeps routes and rules in ONE
// revisioned document (component/memql/policy_customization.go,
// PolicyDocument.Revision): a route save or reset and a rule validate, save or
// removal each name the revision they read, and every write bumps it. So the
// routes and the rules are read HERE, once, and every write in either tab --
// landed or refused -- re-reads BOTH (`reloadAll`). A tab that re-read only
// itself would leave the other one a revision behind, and its next write
// would be refused every time.
//
// THE TABS REACH EACH OTHER. A rule a shipped rule already decides points at
// the route to change instead ("Change Fast local first"), and a route says
// how many rules take it, opening Rules on exactly those. Both arrive as a
// request carrying a counter, so asking twice for the same thing still moves.

export type RoutingTab = "routes" | "rules" | "machines" | "history";

const TAB_NAMES: Record<RoutingTab, string> = { routes: "Routes", rules: "Rules", machines: "Machines", history: "History" };

export function RoutingSection({
  onAddMachine,
  onOpenModels,
  intent,
  consumeIntent,
}: {
  /** Opens Fleet's add-a-machine flow; offered in the tray when no machine is connected. */
  onAddMachine?: () => void;
  /** Opens Fleet's Models section, where an embedding model is pulled and bound. */
  onOpenModels?: () => void;
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const { accessEpoch } = useSession(); // re-render when the effective grants change
  void accessEpoch;
  const canCompose = accessAdmits(RULES_SECTION_RESOURCE);
  const canHistory = accessAdmits(DECISIONS_SECTION_RESOURCE);
  const tabs = (["routes", "rules", "machines", "history"] as const).filter(
    (id) => id !== "rules" || canCompose,
  ).filter((id) => id !== "history" || canHistory);

  const [chosen, setChosen] = useState<RoutingTab | null>(null);
  const tab: RoutingTab = chosen !== null && tabs.includes(chosen) ? chosen : tabs[0]!;
  const [visited, setVisited] = useState<RoutingTab[]>([]);
  useEffect(() => {
    setVisited((held) => (held.includes(tab) ? held : [...held, tab]));
  }, [tab]);
  const panes = visited.includes(tab) ? visited : [...visited, tab];

  // ARRIVING AT A TAB. Settings' Rules and Decisions signposts, and anything
  // else that opens Fleet here, name the tab in the intent.
  const wanted = typeof intent?.payload["routingTab"] === "string" ? (intent.payload["routingTab"] as string) : "";
  const wantedRoute = typeof intent?.payload["routingRoute"] === "string" ? (intent.payload["routingRoute"] as string) : "";
  const [modeRequest, setModeRequest] = useState<{ id: string; n: number } | null>(null);
  const [rulesView, setRulesView] = useState<"rules" | "chains">("rules");
  useEffect(() => {
    if (!intent || (wanted === "" && wantedRoute === "")) return;
    if ((tabs as readonly string[]).includes(wanted)) setChosen(wanted as RoutingTab);
    // "chains" is a place one step under Rules, not a tab of its own.
    if (wanted === "chains" && tabs.includes("rules")) {
      setChosen("rules");
      setRulesView("chains");
    } else if (wanted === "rules") setRulesView("rules");
    if (wantedRoute !== "") {
      setChosen("routes");
      setModeRequest((held) => ({ id: wantedRoute, n: (held?.n ?? 0) + 1 }));
    }
    consumeIntent?.(intent.id);
  }, [intent?.id, wanted, wantedRoute]);

  const [epoch, setEpoch] = useState(0);
  const catalog = useTaskPolicies(epoch, canCompose);
  // History names a decision's rule by what it matches, so it reads the rules
  // too -- it is gated on its own capability, which admits admins.
  const rules = useRules(canCompose || canHistory);
  const reloadRules = rules.reload;
  const reloadAll = useCallback(() => {
    setEpoch((n) => n + 1);
    reloadRules();
  }, [reloadRules]);
  const like = useMemo(() => routesLike(catalog.policies), [catalog.policies]);
  const capacity = useFleetCapacity(true);
  const facts = useRoutingFacts(like, true, capacity.available ? capacity.value : null);

  // RULES AND THEIR CHAINS ARE ONE TAB: Chains opens one step under Rules,
  // and each reaches the other ("Change Fast local first", "Taken by 3 rules").
  const [chainRequest, setChainRequest] = useState<{ name: string; n: number } | null>(null);
  const [rulesRequest, setRulesRequest] = useState<{ route: string; n: number } | null>(null);
  const openChain = useCallback((name: string) => {
    setChainRequest((held) => ({ name, n: (held?.n ?? 0) + 1 }));
    setRulesView("chains");
    setChosen("rules");
  }, []);
  const openRulesFor = useCallback((route: string) => {
    setRulesRequest((held) => ({ route, n: (held?.n ?? 0) + 1 }));
    setRulesView("rules");
    setChosen("rules");
  }, []);

  const header = (actions: ReactNode, meta?: ReactNode) => (
    <div className="fleet-routing-header">
      <Head title="Routing" meta={meta}>
        {actions}
        {capacity.available ? <AutoRefresh busy={false} onRefresh={capacity.reread} intervalMs={15_000} /> : null}
      </Head>
      {tabs.length > 1 ? (
        <LocalTabs label="Routing views" value={tab} onChange={setChosen} options={tabs.map((id) => [id, TAB_NAMES[id]] as const)} />
      ) : null}
    </div>
  );

  return (
    <div className="fleet-routing" data-os-page-context={JSON.stringify({ page: "Routing", view: TAB_NAMES[tab] })}>
      {panes.map((id) => (
        <div key={id} className="fleet-routing-tab" data-routing-tab={id} hidden={id !== tab}>
          {id === "routes" ? (
            <RoutesTab header={header} facts={facts} composes={canCompose} request={modeRequest} onAddMachine={onAddMachine} onOpenModels={onOpenModels} />
          ) : id === "rules" ? (
            rulesView === "chains" ? (
              <ChainsTab
                onBack={() => setRulesView("rules")}
                catalog={catalog}
                facts={facts}
                rules={rules}
                reload={reloadAll}
                request={chainRequest}
                onOpenRules={openRulesFor}
                onAddMachine={onAddMachine}
              />
            ) : (
              <RulesTab
                header={(actions, meta) =>
                  header(
                    <>
                      <button type="button" className="os-link fleet-rules-chains" onClick={() => setRulesView("chains")}>Chains</button>
                      {actions}
                    </>,
                    meta,
                  )
                }
                rules={rules}
                routes={catalog.policies}
                facts={facts}
                onChanged={reloadAll}
                onOpenRoute={openChain}
                request={rulesRequest}
              />
            )
          ) : id === "machines" ? (
            <MachinesTab header={header} />
          ) : (
            <HistoryTab header={header} rules={rules} />
          )}
        </div>
      ))}
    </div>
  );
}
