import { IdentityScreen } from "../../auth/IdentityScreen";
import { useSession } from "../../chrome/access";
import { Button } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { ProfileSection } from "../identity/ProfileSection";
import { AppsIndexSection } from "../settings/AppsIndexSection";
import { DiagnosticsSection } from "../settings/DiagnosticsSection";
import { ConnectionHistoryProvider } from "../settings/useConnectionHistory";
import { AboutSection, AppearanceSection, AskSection } from "./PersonalSections";

/** Personal controls use the same self-scoped services as before the app split.
 * Opening Profile grants no account authority; switching accounts will require
 * the server's account-bound session contract, never a local selection. */
export function ProfileApp(props: OsAppProps) {
  const { signOut } = useSession();
  return <ConnectionHistoryProvider>
    {sectionFor(props)}
    {signOut ? <div className="os-panel-actions"><Button onClick={signOut}>Sign out</Button></div> : null}
  </ConnectionHistoryProvider>;
}

function sectionFor({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "devices") return <IdentityScreen key="devices" initialPath="/me/devices" embedded />;
  if (sectionId === "tokens") return <IdentityScreen key="tokens" initialPath="/me/tokens" embedded />;
  if (sectionId === "settings") return <IdentityScreen key="settings" initialPath="/me/settings" embedded />;
  if (sectionId === "appearance") return <AppearanceSection />;
  if (sectionId === "ask") return <AskSection />;
  if (sectionId === "apps") return <AppsIndexSection />;
  if (sectionId === "about") return <AboutSection />;
  if (sectionId === "diagnostics") return <DiagnosticsSection />;
  if (sectionId === "logs") return <AppLogsSection app="profile" intent={intent} consumeIntent={consumeIntent} />;
  return <ProfileSection />;
}
