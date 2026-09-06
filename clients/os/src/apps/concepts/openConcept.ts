// Arriving at MemQL OS with a concept already named (epic memql#5009).
//
// ===========================================================================
// WHAT THIS IS FOR
// ===========================================================================
// The VS Code extension's Constructs view has a concept selected and wants
// to show somebody its rows. There is no in-editor row browser and there is
// deliberately not going to be one, so the handoff goes the other way: the
// extension opens the console at that concept.
//
// The portal answered a ROUTE for this (`/concepts/:id`). MemQL OS is a
// desktop shell and has no router -- a window carries an app and a section,
// not a path -- so the equivalent is a query parameter read once at boot and
// turned into an open intent, which is exactly what the GitHub-connect
// return already does (`apps/deployables/sources/connectReturn.ts`).
//
// This is modelled on that pair line for line, including where each half
// lives: the capture runs at module scope in `main.tsx` so it is strictly
// earlier than AuthProvider's own read of the query string, and the
// dispatcher lives in THIS app's tree rather than in the shell, because
// knowing that concepts are a Concepts-app surface is this app's knowledge.
//
// ===========================================================================
// WHY IT IS SCRUBBED
// ===========================================================================
// The parameter is spent the moment it is read. Leaving it in the address
// bar means a reload replays it -- reopening a window somebody closed -- and
// means the back button walks into a URL whose meaning is gone. `replaceState`
// rather than `pushState` for the same reason.

/** The query parameter the extension composes. */
export const CONCEPT_OPEN_PARAM = "concept";

/**
 * Read a concept id out of a query string, or null when there is no marker.
 *
 * A marker with an EMPTY value is NOT a request: "open the console at
 * nothing" is a malformed link, and honouring it by opening the app on its
 * list would make a broken link indistinguishable from a working one.
 */
export function readConceptOpen(search: string): string | null {
  const params = new URLSearchParams(search);
  if (!params.has(CONCEPT_OPEN_PARAM)) return null;
  const id = (params.get(CONCEPT_OPEN_PARAM) ?? "").trim();
  return id === "" ? null : id;
}

/** The same query with this parameter removed, leading `?` included, or ""
 *  when nothing else was there. */
export function scrubbedConceptSearch(search: string): string {
  const params = new URLSearchParams(search);
  params.delete(CONCEPT_OPEN_PARAM);
  const rest = params.toString();
  return rest === "" ? "" : `?${rest}`;
}

let parked: string | null = null;

/**
 * Read the marker, scrub it from the address bar, and park it.
 *
 * Called ONCE from `main.tsx`, at module scope, before React renders. It
 * removes only its own parameter and leaves the path, the hash and
 * everything else alone -- the connect return's rule, and the reason two
 * boot-time readers of the same query string cannot eat each other's
 * parameters.
 */
export function captureConceptOpen(win: Window): string | null {
  const conceptId = readConceptOpen(win.location.search);
  if (conceptId === null) return null;
  parked = conceptId;
  const next = `${win.location.pathname}${scrubbedConceptSearch(win.location.search)}${win.location.hash}`;
  win.history.replaceState({}, "", next);
  return conceptId;
}

/**
 * Take the parked concept, once.
 *
 * ONCE is the whole contract: the effect that consumes this runs again on a
 * StrictMode remount and on any re-render that re-registers it, and a value
 * that survived would reopen the window every time.
 */
export function takeParkedConceptOpen(): string | null {
  const held = parked;
  parked = null;
  return held;
}

/** Tests only: forget anything parked, so one case cannot leak into the next. */
export function clearParkedConceptOpen(): void {
  parked = null;
}
