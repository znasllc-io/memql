import type { ReactNode } from "react";

import { clusterLabelFor } from "../cluster/endpoint";

// Which cluster this is.
//
// Under the derive-from-origin registry decision (src/cluster/endpoint.ts), the
// cluster IS the origin, so its host is its name -- and a name the operator
// already recognises, because they typed it. Rendered as the header's title
// rather than tucked into a tooltip: a console that does not say which system
// it is pointed at is how someone runs the right action against the wrong one.
//
// The individual replica serving this stream is a different fact, and it lives
// next door on the ConnectionIndicator (the node id from the ServerHello) --
// useful in a two-replica mesh where one pod misbehaves.

export function ClusterBadge(): ReactNode {
  const cluster = clusterLabelFor(globalThis.location);
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <span className="text-lg font-semibold tracking-tight">memQL Portal</span>
      {cluster ? (
        <span className="truncate font-mono text-sm text-muted" title={cluster}>
          {cluster}
        </span>
      ) : null}
    </div>
  );
}
