import { ArrowUpRight } from "lucide-react";

import { EmptyState } from "../../kit";
import { useAppReach } from "../../kit/ReadinessStates";

// Routing lives in Fleet (routing redesign, 2026-09-28). Settings keeps what
// is a credential or a cluster-wide value -- vendor ids, the Levels table --
// and says ONCE, quietly, where the rest went.

/** The one quiet link a Settings AI page carries: "Routing is in Fleet". */
export function RoutingIsInFleet({ tab }: { tab?: "routes" | "rules" | "machines" | "history" }) {
  const fleet = useAppReach("fleet");
  if (!fleet.sections.includes("routing")) return null;
  if (!fleet.canOpenWindows) return <p className="os-caption">Routing is in Fleet.</p>;
  return (
    <button type="button" className="fleet-reading-link os-routing-link" onClick={() => fleet.open("routing", tab ? { routingTab: tab } : undefined)}>
      Routing is in Fleet
      <ArrowUpRight size={14} aria-hidden />
    </button>
  );
}

/**
 * The one quiet link Settings > Vendors carries to a person's own API keys.
 *
 * Settings > Vendors is the cluster's own account at each vendor, and no key
 * is entered there. A key of a person's own is added in Fleet (design record
 * 2026-11-21-vendor-accounts-and-api-keys, U6), so the page says so once and
 * takes them there. Drawn only for somebody who can open that section: a
 * signpost to a place a person cannot go is not a signpost.
 */
export function ApiKeysAreInFleet() {
  const fleet = useAppReach("fleet");
  if (!fleet.sections.includes("keys")) return null;
  if (!fleet.canOpenWindows) return <p className="os-caption">API keys are in Fleet.</p>;
  return (
    <button type="button" className="fleet-reading-link os-routing-link" onClick={() => fleet.open("keys")}>
      API keys are in Fleet
      <ArrowUpRight size={14} aria-hidden />
    </button>
  );
}

/**
 * Where Settings > Rules and Settings > Decisions used to be.
 *
 * The two sections are drill-downs now, off the rail (registry.tsx says why
 * they are still declared), so a person lands here only by an old link. The
 * page says where the thing went and takes them there.
 */
export function RoutingMovedSection({ what }: { what: "rules" | "decisions" }) {
  const title = what === "rules" ? "Rules" : "Decisions";
  return (
    <div className="os-settings">
      <EmptyState title={`${title} moved to Fleet`} action={<RoutingIsInFleet tab={what === "rules" ? "rules" : "history"} />}>
        {what === "rules" ? "Rules are under Fleet > Routing, beside the routes they choose." : "What served each call is under Fleet > Routing > History."}
      </EmptyState>
    </div>
  );
}
