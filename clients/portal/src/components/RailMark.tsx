import type { ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { useRecentActivity } from "../cluster/activity";
import { MemqlMark } from "./MemqlMark";

// The mark IS the connection (memql#4180). The rail's graph polyhedron
// renders the stream state an operations console must never hide:
//
//   streaming   connected and data just arrived -- the nodes pulse in a
//               quiet stagger, the graph visibly alive.
//   connected   connected, idle -- still, full accent.
//   connecting  the whole mark breathes while the dial is in flight.
//   offline     dimmed and desaturated: no stream (idle, closed, or error).
//
// The states are CSS driven off data-conn (src/styles/index.css), so
// prefers-reduced-motion can flatten every animation to the static colour
// states in one media query. The accessible name states the fact in words --
// the animation is redundant with it, never the only carrier.

const LABELS = {
  streaming: "Connected — live updates arriving",
  connected: "Connected to the cluster",
  connecting: "Connecting to the cluster",
  offline: "Not connected to the cluster",
} as const;

export function RailMark({ size = 24 }: { size?: number }): ReactNode {
  const { status } = useCluster();
  const active = useRecentActivity();

  const conn =
    status === "connected"
      ? active
        ? "streaming"
        : "connected"
      : status === "connecting"
        ? "connecting"
        : "offline";

  return (
    <span className="mark-connection text-accent" data-conn={conn}>
      <MemqlMark size={size} title={LABELS[conn]} />
    </span>
  );
}
