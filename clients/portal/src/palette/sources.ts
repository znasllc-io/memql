import { useMemo } from "react";

import { useAdminAccess } from "../admin/useAdminConsole";
import { conceptPath } from "../concepts/urls";
import { composePath, composedViewPath } from "../compose/urls";
import { useSavedViews } from "../compose/useSavedViews";
import { useConcepts } from "../cluster/useConcepts";
import { DESTINATIONS, destinationPath, visibleTabs } from "../app/nav";
import { fleetPath } from "../fleet/urls";
import { VIEWS } from "../views/registry";
import { viewPath } from "../views/urls";

// Everything the palette can take you to.
//
// ===========================================================================
// A CONSUMER OF THE NAV, NEVER A SECOND COPY OF IT
// ===========================================================================
// The destinations and every tab come out of src/app/nav.ts, which is also
// what the rail renders and what the guide-coverage gate walks. A palette
// with its own list would be the same failure the old rail had one level up:
// two lists of one thing, and the one nobody edits goes stale silently.
//
// ===========================================================================
// NO NEW WIRE SURFACE
// ===========================================================================
// Every source is data the portal already fetches. Composed views ride the
// same subscription-backed read the gallery uses, so a view saved in another
// tab is in the palette without a reload -- which matters more here than
// anywhere: the palette is where a person with forty views goes to find one.
// Concepts ride the registry delta stream the concept browser already holds
// open.

export interface PaletteEntry {
  readonly id: string;
  readonly label: string;
  // The band it appears under. Also the thing that makes an unfiltered
  // palette readable -- seven destinations, then tabs, then views.
  readonly group: string;
  // Secondary text, and a weak match target. A path, a concept's domain.
  readonly hint?: string;
  readonly to: string;
}

// The page-level actions.
//
// EACH ONE GOES SOMEWHERE IT CAN ACTUALLY BE DONE. "Add machine" opens the
// machines page WITH the form open (?add=1); the other two open the surface
// that offers them. What none of them do is pretend to perform the act from
// here -- a palette that half-did things would be a second, worse version of
// each of those forms.
function actionsFor(canAdminister: boolean): readonly PaletteEntry[] {
  const actions: PaletteEntry[] = [
    { id: "action.newView", label: "New view", group: "Actions", hint: "compose a screen over your data", to: composePath() },
    { id: "action.addMachine", label: "Add machine", group: "Actions", hint: "pair a computer you own", to: `${fleetPath("machines")}?add=1` },
  ];
  if (canAdminister) {
    // Inviting is an admin action on the Users view, so the entry is offered
    // to exactly the people the page offers the control to. An entry that
    // landed a reader on a page with no Invite button would be a promise the
    // product does not keep.
    actions.push({
      id: "action.invite",
      label: "Invite someone",
      group: "Actions",
      hint: "send somebody a way in",
      to: viewPath("users"),
    });
  }
  return actions;
}

export function usePaletteEntries(): readonly PaletteEntry[] {
  const { role, canAdminister } = useAdminAccess();
  const saved = useSavedViews();
  const { concepts } = useConcepts();

  return useMemo(() => {
    const entries: PaletteEntry[] = [];

    for (const destination of DESTINATIONS) {
      entries.push({
        id: `go.${destination.id}`,
        label: destination.label,
        group: "Go to",
        to: destinationPath(destination, role),
      });
      const path = destinationPath(destination, role);
      for (const tab of visibleTabs(destination, role)) {
        // Skipped only when the tab IS the destination -- same name, same
        // place -- which would be two identical rows. A tab whose NAME
        // differs is kept even when it is the area's only one: for a reader
        // the Cluster row goes to Integrations, and typing "integrations"
        // should find it.
        if (tab.label === destination.label && tab.to === path) continue;
        entries.push({
          id: `tab.${tab.id}`,
          label: tab.label,
          group: destination.label,
          to: tab.to,
        });
      }
    }

    for (const view of VIEWS) {
      entries.push({ id: `view.${view.id}`, label: view.label, group: "Views", to: viewPath(view.id) });
    }
    for (const view of saved.views) {
      entries.push({
        id: `saved.${view.id}`,
        label: view.name,
        group: "Your views",
        ...(view.description === "" ? {} : { hint: view.description }),
        to: composedViewPath(view.id),
      });
    }

    for (const concept of concepts) {
      entries.push({
        id: `concept.${concept.id}`,
        // The ENTITY is what a person types; the full id is the hint, which
        // is also how "node" finds v1:cluster:node without the reader having
        // to know the domain.
        label: concept.entity,
        group: "Concepts",
        hint: concept.id,
        to: conceptPath(concept.id),
      });
    }

    entries.push(...actionsFor(canAdminister));
    return entries;
  }, [role, canAdminister, saved.views, concepts]);
}
