import { ConnectionsPanel } from "../../modules/connections/ConnectionsPanel";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { OriginsSection } from "../cluster/origins/OriginsSection";
import { IntegrationsSection } from "./IntegrationsSection";
import { VendorsSection } from "./VendorsSection";
import { LevelsSection } from "./LevelsSection";
import { ProceduresSection } from "./ProceduresSection";
import { RoutingMovedSection } from "./RoutingLink";

/** Installation services and data connections. Personal preferences live in
 * Profile, while identity administration, facts and language have their own apps. */
export function SettingsApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "connections") return <ConnectionsPanel appId="settings" intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "data") return <OriginsSection />;
  if (sectionId === "providers") return <VendorsSection intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "levels") return <LevelsSection />;
  if (sectionId === "rules") return <RoutingMovedSection what="rules" />;
  if (sectionId === "decisions") return <RoutingMovedSection what="decisions" />;
  if (sectionId === "procedures") return <ProceduresSection />;
  if (sectionId === "logs") return <AppLogsSection app="settings" subjectConcepts={["v1:platform:dataOrigin", "v1:platform:syncState", "v1:platform:outboxEntry"]} intent={intent} consumeIntent={consumeIntent} />;
  return <IntegrationsSection intent={intent} consumeIntent={consumeIntent} />;
}
