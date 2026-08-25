import { Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { NotFoundPage } from "../pages/NotFoundPage";
import { AccountTab } from "./AccountTab";
import { MeLayout } from "./MeLayout";
import { SecurityTab } from "./SecurityTab";
import { SessionsTab } from "./SessionsTab";
import { SettingsTab } from "./SettingsTab";
import { useMe } from "./useMe";

// The profile surface (memql#4318), mounted from the route table as a SPLAT
// (`me/*`) the way every other feature directory is. Adding a facet is a row
// in urls.ts plus a Route here, and no edit outside this directory.
//
// # One useMe for the whole surface, hoisted here
//
// The account read backs all four facets -- the header's name and role, the
// Account facts, the Security tab's policy switch, and the Settings tab's
// preference bag -- so it is read ONCE at the layout and passed down. A hook
// per tab would re-run the query on every tab change, which is a visible
// flicker on a page whose whole content is four short facts.
//
// Settings rides that read rather than adding its own: `preferences` is
// already on the `userFull` shape currentUser projects, so a second query
// would ask for a field this one has in hand (memql#4523).
//
// Sessions is the exception and reads its own (useMySessions): it is the one
// facet whose data is expensive enough to be worth not fetching until
// somebody asks for it, and it refreshes on its own schedule after a revoke.
//
// # Nothing here is an authorization gate
//
// Every read behind these tabs is SELF-SCOPED server-side -- currentUser,
// passkeysForSelf and authSessionsForSelf resolve their row set from
// actor.userId and take no user id at all -- and every write resolves its
// target from the verified caller rather than from an argument, the Settings
// tab's updateMyPreferences and toggleComputerUseEnabled included. This module
// decides what renders, not what is permitted.

export function MeRoutes(): ReactNode {
  const me = useMe();

  return (
    <MeLayout account={me.account} loading={me.loading}>
      <Routes>
        <Route index element={<AccountTab me={me} />} />
        <Route path="settings" element={<SettingsTab me={me} />} />
        <Route path="sessions" element={<SessionsTab />} />
        <Route path="security" element={<SecurityTab me={me} />} />
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </MeLayout>
  );
}
