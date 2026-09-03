import { useEffect, useRef } from "react";

import { setCaptureContext } from "../logs/capture";
import { sectionsForRole } from "../system/registry";
import { useOs } from "./state";

// The capture's answer to "where did this line come from" (epic memql#4895,
// spec H "Capture"): the focused window's app and section, and the page
// path, read at capture time through a function this installs once.
//
// READ THROUGH A REF, INSTALLED ONCE. The capture holds a function, not a
// value: a line is stamped with the window focused at the moment it is
// recorded, which is a fact about that moment. Re-installing the function on
// every state change would be correct and pointless; a value captured at
// install would be wrong the moment somebody clicked another window.
//
// A window whose section is "" opened on the shell default, which WindowFrame
// resolves to the first admitted section; the same resolution runs here so a
// line names the section that is actually on screen.

export function CaptureContextInstaller() {
  const os = useOs();
  const ref = useRef(os);
  ref.current = os;

  useEffect(() => {
    setCaptureContext(() => {
      const { state, registry, actorRole } = ref.current;
      const focusedId = state.shell.focusedWindowId;
      const win = focusedId === null ? undefined : state.shell.windows[focusedId];
      let section = win?.sectionId ?? "";
      if (win !== undefined && section === "") {
        const manifest = registry.apps.find((a) => a.id === win.appId);
        section = manifest === undefined ? "" : (sectionsForRole(manifest, actorRole)[0]?.id ?? "");
      }
      const href =
        typeof location === "undefined" ? "" : `${location.pathname}${location.search}${location.hash}`;
      return { app: win?.appId ?? "", section, href };
    });
    return () => setCaptureContext(null);
  }, []);

  return null;
}
