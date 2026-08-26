import type { ReactNode } from "react";

import { SettingsTab } from "../me/SettingsTab";
import { useMe } from "../me/useMe";

// Your own preferences, as a widget (epic memql#4661).
//
// The exemplar NON-DATA page, per the epic's own directive: "we're not gonna
// have a custom page for one thing -- even the profile page". A settings tab
// is a form over one row that is yours, and once it is a widget the Me tabs
// are arrangements like everything else.
export function ProfilePreferencesWidget(): ReactNode {
  const me = useMe();
  return <SettingsTab me={me} />;
}
