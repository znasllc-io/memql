import { ConnectionsPanel } from "../../modules/connections/ConnectionsPanel";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { OriginsSection } from "../cluster/origins/OriginsSection";
import { ApiKeysSection } from "./apikeys/ApiKeysSection";
import { IntegrationsSection } from "./IntegrationsSection";
import { VendorsSection } from "./VendorsSection";
import { LevelsSection } from "./LevelsSection";
import { ProceduresSection } from "./ProceduresSection";
import { RoutingMovedSection } from "./RoutingLink";

/** Installation services and data connections, and the accounts that pay for
 * a vendor's models: the cluster's under Vendors, a person's own under API
 * keys. Personal preferences live in Profile, while identity administration,
 * facts and language have their own apps. */
export function SettingsApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "connections") return <ConnectionsPanel appId="settings" intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "data") return <OriginsSection />;
  if (sectionId === "providers") return <VendorsSection intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "apikeys") return <ApiKeysSection />;
  if (sectionId === "levels") return <LevelsSection />;
  if (sectionId === "rules") return <RoutingMovedSection what="rules" />;
  if (sectionId === "decisions") return <RoutingMovedSection what="decisions" />;
  if (sectionId === "procedures") return <ProceduresSection />;
  if (sectionId === "logs") return <AppLogsSection app="settings" subjectConcepts={["v1:platform:dataOrigin", "v1:platform:syncState", "v1:platform:outboxEntry"]} intent={intent} consumeIntent={consumeIntent} />;
  return <IntegrationsSection intent={intent} consumeIntent={consumeIntent} />;
}
