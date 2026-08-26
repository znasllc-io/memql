import type { ReactNode } from "react";

import { AccountTab } from "../me/AccountTab";
import { SecurityTab } from "../me/SecurityTab";
import { SessionsTab } from "../me/SessionsTab";
import { useMe } from "../me/useMe";

// The Me tabs' bodies, as widgets (epic memql#4661, task memql#4674).
//
// Each is a CONTROL SURFACE over one row that is yours -- your account, your
// credentials, your sessions -- which is exactly what a widget is for. The
// convergence here is the page's layout, its version strip and its regenerate
// control, not a rewrite of what the tabs do.
//
// `useMe` is called PER WIDGET rather than threaded down. That is safe here in
// a way it is not on the fleet pages: useMe is a plain read with no
// subscription, and exactly one of these renders at a time (they are routes).
export function MeAccountWidget(): ReactNode {
  return <AccountTab me={useMe()} />;
}

export function MeSecurityWidget(): ReactNode {
  return <SecurityTab me={useMe()} />;
}

export function MeSessionsWidget(): ReactNode {
  return <SessionsTab />;
}
