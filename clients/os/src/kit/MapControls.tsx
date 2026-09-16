import { Minus, Plus, RotateCcw } from "lucide-react";
import { IconButton } from "./IconButton";
import type { PanZoom } from "./usePanZoom";

/** Shared map controls. Wheel scrolling belongs to the containing page. */
export function MapControls({ zoomIn, zoomOut, reset }: Pick<PanZoom, "zoomIn" | "zoomOut" | "reset">) {
  return <div className="os-map-controls" role="group" aria-label="Map view controls">
    <IconButton label="Zoom in" onClick={zoomIn}><Plus size={16} aria-hidden /></IconButton>
    <IconButton label="Zoom out" onClick={zoomOut}><Minus size={16} aria-hidden /></IconButton>
    <IconButton label="Reset zoom and position" onClick={reset}><RotateCcw size={16} aria-hidden /></IconButton>
  </div>;
}
