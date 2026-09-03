import { useEffect } from "react";

import { useOs } from "../../../chrome/state";
import { takeParkedConnectReturn } from "./connectReturn";

// Hands a parked GitHub-connect return to the surface that asked for it
// (epic memql#4915).
//
// ===========================================================================
// IT RENDERS NOTHING, ON PURPOSE
// ===========================================================================
// The result of a connection belongs beside the control somebody pressed --
// the Sources group, or the Source stop when its rebuild lands -- so this
// component's whole job is to open that app on that section and hand the
// answer over as a window intent. A result page of its own would be a toast
// with more pixels, and it would sit in front of a person who has already
// got what they came for.
//
// IT LIVES INSIDE `OsProvider` because opening an app is a shell act, and it
// lives in the DEPLOYABLES tree because knowing that Sources is a Deployables
// surface is this app's knowledge and not the shell's.
//
// The marker was read and scrubbed at boot (`connectReturn.ts`), which is
// strictly earlier than this: the Shell mounts only once somebody is signed
// in, and a return that arrived on a signed-out browser waits through the
// sign-in rather than being lost to it.

export function ConnectReturnDispatcher() {
  const { actions } = useOs();
  useEffect(() => {
    // TAKE, not read: the parked value is consumed here and this effect is
    // free to run again -- a StrictMode remount does, and so does any change
    // in `actions` identity -- and every later run correctly finds nothing.
    const result = takeParkedConnectReturn();
    if (result === null) return;
    actions.openApp("deployables", result.section, { connect: result });
  }, [actions]);
  return null;
}
