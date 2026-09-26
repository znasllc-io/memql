import type { CSSProperties } from "react";

/** The approved Fold wallpaper. CSS owns the two static tonal planes. */
export function FoldWallpaper({ style }: { style?: CSSProperties }) {
  return <span className="os-wallpaper" data-os-wallpaper style={style} aria-hidden="true" />;
}

/** The ghost desk numeral — its placement and treatment are unchanged. */
export function DeskNumeral({ index }: { index: number }) {
  return (
    <div className="os-desk-numeral" data-os-desk-numeral aria-hidden="true">
      {String(index + 1).padStart(2, "0")}
    </div>
  );
}
