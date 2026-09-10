import { ListChecks } from "lucide-react";

import type { OsWidgetManifest } from "../../system/registry";
import { SetupGate } from "./SetupGate";
import { SetupWidget } from "./SetupWidget";

// The first-run wizard's manifest (design record
// 2026-09-06-first-run-wizard, D1).
//
// GATED OWNER-OR-DEVELOPER, exactly as the Integrations section is: these are
// the two roles that can act on any of the four stops, and offering a rail of
// destinations to somebody who will be refused at all four is worse than not
// offering it.
//
// It declares NO `requires`. A widget's `requires` renders the setup sentence
// in its body when a module is missing -- which for this widget would be a
// setup surface inside a setup surface, about the module it exists to set up.
/** Desk footprint shared by Set up and Ask so both primary cards match. */
export const SETUP_WIDGET_SIZE = { w: 4, h: 4 } as const;

export const setupWidget: OsWidgetManifest = {
  id: "setup",
  name: "Set up",
  roles: { any: ["owner", "developer"] },
  icon: ListChecks,
  // FOUR CELLS TALL BECAUSE THE INFERENCE STOP IS, and the visual pass is
  // what settled it: at three, the three doors and the act sat below the
  // fold of a card whose whole job is to offer them. Four holds every stop
  // with one open and nothing scrolled. A desk too short for it places the
  // widget wherever it fits instead -- `addItem` settles on the nearest free
  // cell -- which is a worse position and not a missing wizard.
  //
  // Exported so Ask shares the same desk footprint (production desk parity).
  size: SETUP_WIDGET_SIZE,
  component: SetupWidget,
  gate: SetupGate,
};
