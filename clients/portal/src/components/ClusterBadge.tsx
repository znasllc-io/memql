import type { ReactNode } from "react";

import { clusterLabelFor } from "../cluster/endpoint";
import { MemqlMark } from "./MemqlMark";

// Which cluster this is.
//
// Under the derive-from-origin registry decision (src/cluster/endpoint.ts), the
// cluster IS the origin, so its host is its name -- and a name the operator
// already recognises, because they typed it. Rendered as the header's title
// rather than tucked into a tooltip: a console that does not say which system
// it is pointed at is how someone runs the right action against the wrong one.
//
// The wordmark is the display face's one chrome appearance (Squada One is
// wordmark + big-number moments only, memql#4177); the mark beside it is the
// brand's graph polyhedron in the accent colour. The individual replica
// serving this stream is a different fact, and it lives next door on the
// ConnectionIndicator (the node id from the ServerHello).

export function ClusterBadge(): ReactNode {
  const cluster = clusterLabelFor(globalThis.location);
  return (
    <div className="flex min-w-0 items-center gap-2.5">
      <span className="text-accent">
        <MemqlMark size={20} />
      </span>
      <span className="font-display text-lg leading-none tracking-wide">
        MemQL Portal
      </span>
      {cluster ? (
        <span className="truncate font-mono text-sm text-muted" title={cluster}>
          {cluster}
        </span>
      ) : null}
    </div>
  );
}
