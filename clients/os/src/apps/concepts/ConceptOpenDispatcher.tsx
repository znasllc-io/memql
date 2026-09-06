import { useEffect } from "react";

import { useOs } from "../../chrome/state";
import { takeParkedConceptOpen } from "./openConcept";

// Hands a parked concept-open request to the Concepts app (epic memql#5009).
//
// Renders nothing, on purpose, and does nothing at all on a browser that did
// not arrive with the marker -- which is every other one. It sits inside
// `OsProvider` because opening an app is a shell act, and it lives in the
// CONCEPTS tree because knowing which app serves a concept is this app's
// knowledge and not the shell's -- the reasoning ConnectReturnDispatcher
// records for the same shape.
//
// The marker was read and scrubbed at boot, which is strictly earlier than
// this: the Shell mounts only once somebody is signed in, so a link opened
// on a signed-out browser waits through the sign-in rather than being lost
// to it.
//
// A person whose role does not admit the Concepts app opens nothing:
// `openApp` refuses an app the actor cannot see, which is the same one
// admission check the dock and the launcher go through. A link is not a way
// past it.

export function ConceptOpenDispatcher() {
  const { actions } = useOs();
  useEffect(() => {
    // TAKE, not read: consumed here, so every later run correctly finds
    // nothing -- including the StrictMode remount, which would otherwise
    // open a second window.
    const conceptId = takeParkedConceptOpen();
    if (conceptId === null) return;
    actions.openApp("concepts", "registry", { conceptId });
  }, [actions]);
  return null;
}
