import type { ReactNode } from "react";
import { InfoDetail } from "./InfoDetail";

export function MapHeading({ title, children }: { title: string; children?: ReactNode }) {
  return <div className="os-map-heading">
    <h4>{title}</h4>
    <InfoDetail title={title}>
      <p>Drag to pan. Use + and − to zoom, and Reset to restore the original zoom and position. Scrolling moves the page.</p>
      <p>With the map focused, arrow keys pan, + and − zoom, and 0 resets. Tab moves between items; Enter opens one.</p>
      {children}
    </InfoDetail>
  </div>;
}
