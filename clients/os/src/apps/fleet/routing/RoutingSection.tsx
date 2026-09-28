import { useEffect, useMemo, useState, type ReactNode } from "react";
import "./routing.css";

import { Head } from "../../../kit";
import { LocalTabs } from "../../../kit/LocalTabs";
import { useSession } from "../../../chrome/access";
import { accessAdmits, type OsAppProps } from "../../../system/registry";
import { useTaskPolicies } from "../taskPolicies";
import { DECISIONS_SECTION_RESOURCE, RULES_SECTION_RESOURCE } from "./access";
import { HistoryTab } from "./HistoryTab";
import { MachinesTab } from "./MachinesTab";
import { routesLike } from "./routes";
import { RoutesTab } from "./RoutesTab";
import { RulesTab } from "./RulesTab";
import { useRoutingFacts } from "./useRoutingFacts";

// Fleet > Routing (design brief 2026-09-28, sections 2-5): ONE home for how a
// model call finds something to answer it. It replaces Fleet's "Policies" and
// "Machine routing" sections and takes in Settings' Rules and Decisions.
//
//   Routes     ordered lists of sources, composed on a route's own page
//   Rules      "when work looks like X, take route Y", in the order tried
//   Machines   which of your machines takes a call a route sent to them
//   History    what actually served recent calls, and why
//
// THE GATES MOVED WITH THE SCREENS. Routes and Rules sit behind
// `app:settings/rules` (owner or developer) and History behind
// `app:settings/decisions` (owner, developer, admin) -- the capabilities that
// gated them in Settings, so nobody's reach changed. A tab a person cannot
// open is absent, never a tab that refuses. Machines is per person and ungated.
//
// VISITED TABS STAY MOUNTED, hidden, so a draft route or a half-written rule
// survives a look at History. Each tab draws the section's Head on its list
// page and its own Head on a drill-down, so there is one Head on screen.

export type RoutingTab = "routes" | "rules" | "machines" | "history";

const TAB_NAMES: Record<RoutingTab, string> = { routes: "Routes", rules: "Rules", machines: "Machines", history: "History" };

export function RoutingSection({
  onAddMachine,
  intent,
  consumeIntent,
}: {
  /** Opens Fleet's add-a-machine flow; offered in the tray when no machine is connected. */
  onAddMachine?: () => void;
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const { accessEpoch } = useSession(); // re-render when the effective grants change
  void accessEpoch;
  const canCompose = accessAdmits(RULES_SECTION_RESOURCE);
  const canHistory = accessAdmits(DECISIONS_SECTION_RESOURCE);
  const tabs = (["routes", "rules", "machines", "history"] as const).filter(
    (id) => (id !== "routes" && id !== "rules") || canCompose,
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
  useEffect(() => {
    if (!intent || wanted === "") return;
    if ((tabs as readonly string[]).includes(wanted)) setChosen(wanted as RoutingTab);
    consumeIntent?.(intent.id);
  }, [intent?.id, wanted]);

  const [epoch, setEpoch] = useState(0);
  const catalog = useTaskPolicies(epoch, canCompose);
  const like = useMemo(() => routesLike(catalog.policies), [catalog.policies]);
  const facts = useRoutingFacts(like);

  const header = (actions: ReactNode, meta?: ReactNode) => (
    <div className="fleet-routing-header">
      <Head title="Routing" meta={meta}>{actions}</Head>
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
            <RoutesTab header={header} catalog={catalog} facts={facts} reload={() => setEpoch((n) => n + 1)} onAddMachine={onAddMachine} />
          ) : id === "rules" ? (
            <RulesTab header={header} routes={catalog.policies} facts={facts} />
          ) : id === "machines" ? (
            <MachinesTab header={header} />
          ) : (
            <HistoryTab header={header} />
          )}
        </div>
      ))}
    </div>
  );
}
