import type { ReactNode } from "react";
import { MapControls } from "./MapControls";
import { usePanZoom, type FitRequest } from "./usePanZoom";
import { transformOf } from "./viewport";

export interface MapCanvasProps extends FitRequest {
  label: string;
  children: ReactNode;
  /** Drawing-specific styling; framing and controls stay shared. */
  canvasClassName?: string;
  behind?: boolean;
}

/** The shared SVG viewport. Apps supply their drawing in layout coordinates. */
export function MapCanvas({ label, children, canvasClassName, behind, ...fit }: MapCanvasProps) {
  const pan = usePanZoom(fit);
  return <div className="os-map-frame" ref={pan.frameRef} data-behind={behind || undefined}>
    <svg
      className={["os-map-canvas", canvasClassName].filter(Boolean).join(" ")}
      role="application"
      aria-label={label}
      tabIndex={0}
      {...pan.handlers}
      onFocus={event => {
        if (event.target !== event.currentTarget) pan.reveal(event.target.getBoundingClientRect());
      }}
      onClickCapture={event => {
        // A drag can end in a click on the node it started over. Suppress
        // that pointer click while preserving keyboard/assistive activation.
        if (event.detail > 0 && pan.steering()) {
          event.preventDefault();
          event.stopPropagation();
        }
      }}
    >
      <g data-os-map-view transform={transformOf(pan.view)}>{children}</g>
    </svg>
    <MapControls zoomIn={pan.zoomIn} zoomOut={pan.zoomOut} reset={pan.reset} />
  </div>;
}
