import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { MutableRefObject, ReactNode } from "react";
import {
  closestCenter,
  DndContext,
  PointerSensor,
  pointerWithin,
  useSensor,
  useSensors,
  type CollisionDetection,
  type DragEndEvent,
  type DragStartEvent,
} from "@dnd-kit/core";

// ONE DRAG SYSTEM FOR THE WHOLE SHELL (epic memql#4784).
//
// ===========================================================================
// WHY THIS EXISTS: A DRAG THAT LEAVES THE DESK
// ===========================================================================
// The desktop and the dock used to own a DndContext each, and they are
// SIBLINGS in the layout -- so a drag begun on a desk icon could never reach a
// droppable in the dock, because dnd-kit resolves a drop only among the
// droppables inside the context the drag started in. That was invisible while
// every gesture stayed on its own side of the line. The Bin is the first thing
// that crosses it: dragging a file onto a trash can is the gesture people
// already know, and the file is on the desk or in a window while the can is in
// the dock.
//
// The alternative was to give the Bin a second drag mechanism -- native HTML5
// dnd for this one target, dnd-kit for everything else -- and that is exactly
// the shape the OS README warns about for cues: two implementations of one
// behaviour, free to drift, with "the desk icon can be dragged to the Bin but
// the Files row cannot" as the bug nobody notices for a month. The id space
// was already namespaced for this (`window:`, `item:`, `folder:`, `pin:`,
// `pagerdot:`), which is what makes one flat context cheap.
//
// ===========================================================================
// HANDLERS ARE REGISTERED AS REFS, AND THAT IS LOAD-BEARING
// ===========================================================================
// A dnd-kit context takes exactly ONE onDragEnd, and the two halves of the
// shell need entirely different ones -- the desk moves icons and throws
// windows, the dock reorders pins. So consumers claim an id PREFIX and hand
// over a ref whose `.current` this scope reads at drop time.
//
// The ref is what keeps the registration effect's dependencies stable. A hook
// that re-registered whenever its handler closure changed would re-register on
// EVERY render of the desktop -- and its cleanup would unregister the desk
// mid-flight, which presents as a drag that silently does nothing every few
// gestures. Writing `.current` during render instead means the latest closure
// is always the one that runs, and the effect runs once.

export interface ShellDragHandlers {
  onDragStart?: (event: DragStartEvent) => void;
  onDragEnd: (event: DragEndEvent) => void;
}

interface ShellDragApi {
  claim: (prefixes: readonly string[], ref: MutableRefObject<ShellDragHandlers>) => () => void;
  /** The id currently being dragged, or "" -- what a drop target reads to
   *  offer itself before the pointer is over it. */
  activeId: string;
}

const ShellDragContext = createContext<ShellDragApi | null>(null);

interface Claim {
  prefixes: readonly string[];
  ref: MutableRefObject<ShellDragHandlers>;
}

export function ShellDragScope({ children }: { children: ReactNode }) {
  const claims = useRef<Claim[]>([]);
  const [activeId, setActiveId] = useState("");

  // Without an activation distance a draggable treats pointerdown as a drag
  // start and swallows the CLICK -- a pinned app that cannot launch, and a
  // file row that cannot be selected. 6px is the dock's own long-standing
  // value; one sensor set now serves every draggable in the shell.
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 6 } }));

  const claim = useCallback((prefixes: readonly string[], ref: MutableRefObject<ShellDragHandlers>) => {
    const entry: Claim = { prefixes, ref };
    claims.current = [...claims.current, entry];
    return () => {
      claims.current = claims.current.filter((c) => c !== entry);
    };
  }, []);

  const forEach = useCallback((id: string, run: (h: ShellDragHandlers) => void) => {
    for (const c of claims.current) {
      if (c.prefixes.some((p) => id.startsWith(p))) run(c.ref.current);
    }
  }, []);

  const onDragStart = useCallback(
    (event: DragStartEvent) => {
      const id = String(event.active.id);
      setActiveId(id);
      forEach(id, (h) => h.onDragStart?.(event));
    },
    [forEach],
  );

  const onDragEnd = useCallback(
    (event: DragEndEvent) => {
      const id = String(event.active.id);
      setActiveId("");
      forEach(id, (h) => h.onDragEnd(event));
    },
    [forEach],
  );

  // A cancelled drag (Escape, a lost pointer) must clear the active id too,
  // or every target that lit up for it stays lit with nothing in flight.
  const onDragCancel = useCallback(() => setActiveId(""), []);

  // COLLISION DETECTION IS PER-DRAG, because the two gestures want different
  // answers. Reordering a row of pins wants the nearest CENTRE -- the pointer
  // sits between two icons for most of the gesture, and `pointerWithin` would
  // report no target at all there. Everything else wants `pointerWithin`: a
  // desk icon dropped between two folders must land on neither, and nearest-
  // centre would file it into whichever happened to be closer.
  const collisionDetection = useCallback<CollisionDetection>((args) => {
    return String(args.active.id).startsWith("pin:") ? closestCenter(args) : pointerWithin(args);
  }, []);

  const api = useMemo<ShellDragApi>(() => ({ claim, activeId }), [claim, activeId]);

  return (
    <ShellDragContext.Provider value={api}>
      <DndContext
        sensors={sensors}
        collisionDetection={collisionDetection}
        onDragStart={onDragStart}
        onDragEnd={onDragEnd}
        onDragCancel={onDragCancel}
      >
        {children}
      </DndContext>
    </ShellDragContext.Provider>
  );
}

/**
 * Handle drops whose active id begins with one of `prefixes`.
 *
 * `prefixes` must be a stable array -- a literal defined outside the component,
 * or a memo. It is a dependency of the registration effect, and an array
 * rebuilt each render would re-register on every one.
 */
export function useShellDragClaim(prefixes: readonly string[], handlers: ShellDragHandlers): void {
  const api = useContext(ShellDragContext);
  const ref = useRef(handlers);
  // Written during render, deliberately: the effect below must not depend on
  // the handler identity, and this is what lets the drop read the newest
  // closure anyway.
  ref.current = handlers;
  useEffect(() => {
    if (api === null) return;
    return api.claim(prefixes, ref);
  }, [api, prefixes]);
}

/** What is being dragged right now, or "" -- for a target that offers itself
 *  the moment a compatible drag starts rather than when the pointer arrives. */
export function useActiveDrag(): string {
  return useContext(ShellDragContext)?.activeId ?? "";
}
