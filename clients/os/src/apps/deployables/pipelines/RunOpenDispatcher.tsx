import { useEffect } from "react";

import { useOs } from "../../../chrome/state";
import { takeParkedRunOpen } from "./openRun";

// Hands a parked `?pipelineRun=` to Deployables' Runs (epic memql#5479). It
// renders nothing and does nothing on a browser that did not arrive with the
// marker. A person whose access does not admit Deployables, or its Runs
// section, opens nothing: `openApp` and the section gate are the same checks
// the dock and the tabs go through, and a link is not a way past them. The
// run itself is read owner-scoped, so a link to somebody else's run opens on
// a page that says it cannot be read -- never on their run.
export function RunOpenDispatcher() {
  const { actions } = useOs();
  useEffect(() => {
    const runId = takeParkedRunOpen();
    if (runId === null) return;
    actions.openApp("deployables", "runs", { runId });
  }, [actions]);
  return null;
}
