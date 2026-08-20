import type { ReactNode } from "react";

import { clusterLabelFor } from "../cluster/endpoint";

// Which cluster this is.
//
// Under the derive-from-origin registry decision (src/cluster/endpoint.ts),
// the cluster IS the origin, so its host is its name -- and a name the
// operator already recognises, because they typed it. Rendered as the
// topbar's leading fact rather than tucked into a tooltip: a console that
// does not say which system it is pointed at is how someone runs the right
// action against the wrong one.
//
// The BRAND (mark + wordmark) lives in the rail since memql#4179; this badge
// is purely the cluster fact. The individual replica serving this stream is
// a different fact again, and it lives on the ConnectionIndicator (the node
// id from the ServerHello).

export function ClusterBadge(): ReactNode {
  const cluster = clusterLabelFor(globalThis.location);
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <span className="text-xs font-semibold tracking-wide text-subtle uppercase">
        Cluster
      </span>
      <span
        className="truncate font-mono text-sm text-fg"
        {...(cluster ? { title: cluster } : {})}
      >
        {cluster || "unknown origin"}
      </span>
    </div>
  );
}
