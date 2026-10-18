import { IdentityScreen } from "../../auth/IdentityScreen";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { ProfileSection } from "./ProfileSection";

export function IdentityApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "logs") return <AppLogsSection app="identity" intent={intent} consumeIntent={consumeIntent} />;
  // The Profile is native (memql#6220): it reads and writes the person's own
  // row over the cluster stream. The other sections are still the identity
  // service's own pages, drawn in place.
  if (sectionId === "profile" || sectionId === "") return <ProfileSection />;
  const path = sectionId === "tokens" ? "/me/tokens" : sectionId === "settings" ? "/me/settings" : "/me/devices";
  return <IdentityScreen key={path} initialPath={path} embedded />;
}
