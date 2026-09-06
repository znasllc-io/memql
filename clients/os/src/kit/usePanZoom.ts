import { useCallback, useEffect, useRef, useState } from "react";
import type { KeyboardEvent, PointerEvent, RefObject, WheelEvent } from "react";

import {
  IDENTITY,
  KEY_PAN_STEP,
  KEY_ZOOM_STEP,
  fitTo,
  panBy,
  zoomAt,
  type Viewport,
} from "./viewport";

// Steering a map: pointer, touch, wheel and keyboard, over one viewport.
//
// IN `kit/` RATHER THAN IN AN APP, since epic memql#4785, for the reason
// `viewport.ts` beside it is. The arithmetic was already shared; this is the
// GESTURE layer over it, and there are now two maps -- the Deployables map and
// the Nexus beacon map -- that need identical behaviour from it. A second copy
// would be a second set of pointer-capture, pinch-baseline and drag-threshold
// rules, each of which took a bug to get right once.
//
// Nothing in here knows what is being panned. It owns a viewport, a frame
// element and a gesture; the caller owns the picture.

/**
 * Total pointer travel, in screen pixels, past which a press is a DRAG.
 *
 * Manhattan distance rather than Euclidean: it is the cheaper arithmetic and it
 * over-counts slightly, which errs toward treating a wobble as a drag -- and
 * failing to open a node is recoverable in a way that opening one somebody did
 * not ask for, mid-drag, is not.
 */
export const DRAG_THRESHOLD_PX = 5;

export interface PanZoomHandlers {
  onPointerDown: (e: PointerEvent<SVGSVGElement>) => void;
  onPointerMove: (e: PointerEvent<SVGSVGElement>) => void;
  onPointerUp: (e: PointerEvent<SVGSVGElement>) => void;
  onPointerCancel: (e: PointerEvent<SVGSVGElement>) => void;
  onWheel: (e: WheelEvent<SVGSVGElement>) => void;
  onKeyDown: (e: KeyboardEvent<SVGSVGElement>) => void;
}

export interface PanZoom {
  view: Viewport;
  /** Put this on the element that BOUNDS the canvas, not on the svg. */
  frameRef: RefObject<HTMLDivElement | null>;
  /** Spread onto the `<svg>`. */
  handlers: PanZoomHandlers;
  /**
   * Whether the gesture that just ended was steering rather than choosing.
   *
   * A node lives INSIDE the canvas, so a drag that begins on one bubbles to the
   * pan handler and then ends in a `click` on that node -- which would open
   * something somebody was only steering past. The keyboard path does not come
   * through here, so Enter on a node always opens it however far the map was
   * last dragged.
   */
  steering: () => boolean;
  reset: () => void;
}

export interface FitRequest {
  width: number;
  height: number;
  /** False for an empty map: the fit is forgotten so the next one opens framed. */
  ready: boolean;
}

export function usePanZoom(fit: FitRequest): PanZoom {
  const [view, setView] = useState<Viewport>(IDENTITY);
  const frameRef = useRef<HTMLDivElement | null>(null);

  // Pointer events cover mouse, pen and touch with one code path, which is what
  // makes "works with pointer and touch" a property of the implementation
  // rather than of two implementations that have to agree. Two live pointers
  // are a PINCH; one is a drag.
  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const pinch = useRef<{ distance: number; midX: number; midY: number } | null>(null);
  const travelled = useRef(0);

  const localPoint = useCallback((clientX: number, clientY: number) => {
    const box = frameRef.current?.getBoundingClientRect();
    return { x: clientX - (box?.left ?? 0), y: clientY - (box?.top ?? 0) };
  }, []);

  const onPointerDown = useCallback((e: PointerEvent<SVGSVGElement>) => {
    // A PRIMARY pointer down starts a fresh gesture, so anything still in the
    // map is stale -- a pointerup that landed outside the element in a browser
    // with no pointer capture. Without this, the next press would see two live
    // pointers and read a single-finger drag as a pinch, which scales the map
    // by whatever the ghost happened to be sitting at. The second finger of a
    // real pinch is NOT primary, so it never clears the first.
    if (e.isPrimary) pointers.current.clear();
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
    pinch.current = null;
    travelled.current = 0;
    // Capture so a drag that leaves the frame keeps steering. jsdom has no
    // implementation, and a map that threw on pointerdown under test would be a
    // map nothing could test -- hence the optional call rather than a branch.
    try {
      (e.currentTarget as unknown as Element).setPointerCapture?.(e.pointerId);
    } catch {
      // A browser that refuses capture still pans; it just stops at the edge.
    }
  }, []);

  const onPointerMove = useCallback(
    (e: PointerEvent<SVGSVGElement>) => {
      const held = pointers.current.get(e.pointerId);
      if (!held) return;
      const previous = { ...held };
      pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });

      const live = [...pointers.current.values()];
      const a = live[0];
      const b = live[1];
      if (a && b) {
        const distance = Math.hypot(a.x - b.x, a.y - b.y);
        const mid = localPoint((a.x + b.x) / 2, (a.y + b.y) / 2);
        const last = pinch.current;
        pinch.current = { distance, midX: mid.x, midY: mid.y };
        // The FIRST move after a second finger lands only establishes the
        // baseline: a ratio against a distance nobody measured yet would jump
        // the scale by whatever the fingers happened to be apart.
        if (last && last.distance > 0 && distance > 0) {
          setView((v) => zoomAt(v, distance / last.distance, mid.x, mid.y));
        }
        return;
      }

      const dx = e.clientX - previous.x;
      const dy = e.clientY - previous.y;
      travelled.current += Math.abs(dx) + Math.abs(dy);
      setView((v) => panBy(v, dx, dy));
    },
    [localPoint],
  );

  const endPointer = useCallback((e: PointerEvent<SVGSVGElement>) => {
    pointers.current.delete(e.pointerId);
    if (pointers.current.size < 2) pinch.current = null;
  }, []);

  const onWheel = useCallback(
    (e: WheelEvent<SVGSVGElement>) => {
      // deltaY < 0 is a scroll UP, which is a zoom IN everywhere else in every
      // map anybody has used.
      const factor = e.deltaY < 0 ? KEY_ZOOM_STEP : 1 / KEY_ZOOM_STEP;
      const at = localPoint(e.clientX, e.clientY);
      setView((v) => zoomAt(v, factor, at.x, at.y));
    },
    [localPoint],
  );

  const onKeyDown = useCallback((e: KeyboardEvent<SVGSVGElement>) => {
    // The keyboard steers the same viewport the pointer does. It is not a
    // fallback: a map you can only reach with a mouse is a map somebody cannot
    // read at all, and on both surfaces this is the app's signature picture.
    const centre = () => {
      const box = frameRef.current?.getBoundingClientRect();
      return { x: (box?.width ?? 0) / 2, y: (box?.height ?? 0) / 2 };
    };
    switch (e.key) {
      case "ArrowLeft":
        setView((v) => panBy(v, KEY_PAN_STEP, 0));
        break;
      case "ArrowRight":
        setView((v) => panBy(v, -KEY_PAN_STEP, 0));
        break;
      case "ArrowUp":
        setView((v) => panBy(v, 0, KEY_PAN_STEP));
        break;
      case "ArrowDown":
        setView((v) => panBy(v, 0, -KEY_PAN_STEP));
        break;
      case "+":
      case "=": {
        const c = centre();
        setView((v) => zoomAt(v, KEY_ZOOM_STEP, c.x, c.y));
        break;
      }
      case "-":
      case "_": {
        const c = centre();
        setView((v) => zoomAt(v, 1 / KEY_ZOOM_STEP, c.x, c.y));
        break;
      }
      case "0":
        setView(IDENTITY);
        break;
      default:
        return;
    }
    e.preventDefault();
  }, []);

  // ==========================================================================
  // FIT ONCE, THEN NEVER AGAIN
  // ==========================================================================
  // A map that opens clipped is a map somebody has to discover they can pan
  // before they can read it, and a real one is wider than any window it draws
  // in. So the first paint frames the whole thing.
  //
  // ONCE is the whole rule. A row set that changes shape must NOT re-fit:
  // somebody who panned into a corner to read one node would be thrown back to
  // the origin because a step landed somewhere else on the map -- the same
  // class of rudeness as a list that scrolls itself. An empty map resets
  // instead, so the next one that opens starts framed.
  const fitted = useRef(false);
  const { width, height, ready } = fit;
  useEffect(() => {
    if (!ready) {
      fitted.current = false;
      setView(IDENTITY);
      return;
    }
    if (fitted.current) return;
    const box = frameRef.current?.getBoundingClientRect();
    // An unmeasured frame -- a window mid-open, a hidden desk, jsdom, which
    // measures everything as zero -- is not a fit of zero; it is no answer yet,
    // so the attempt is left for a later render rather than being spent on a
    // viewport nobody has laid out.
    if (!box || box.width <= 0 || box.height <= 0) return;
    fitted.current = true;
    setView(fitTo(width, height, box.width, box.height));
  }, [ready, width, height]);

  return {
    view,
    frameRef,
    handlers: {
      onPointerDown,
      onPointerMove,
      onPointerUp: endPointer,
      onPointerCancel: endPointer,
      onWheel,
      onKeyDown,
    },
    steering: () => travelled.current > DRAG_THRESHOLD_PX,
    reset: () => setView(IDENTITY),
  };
}
