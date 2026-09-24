import { IdentityScreen } from "../../auth/IdentityScreen";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";

export function IdentityApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "logs") return <AppLogsSection app="identity" intent={intent} consumeIntent={consumeIntent} />;
  const path = sectionId === "profile" ? "/me/" : sectionId === "tokens" ? "/me/tokens" : sectionId === "settings" ? "/me/settings" : "/me/devices";
  return <IdentityScreen key={path} initialPath={path} embedded />;
}
