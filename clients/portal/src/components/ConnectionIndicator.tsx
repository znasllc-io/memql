import type { ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";

// The header's connection state. Deliberately always visible rather than
// appearing only on failure: "no error shown" and "not looking" are
// indistinguishable to an operator, and this is the one widget that says
// whether anything else on screen can be trusted.

const LABELS: Record<string, string> = {
  connecting: "Connecting",
  connected: "Connected",
  closed: "Disconnected",
  error: "Connection failed",
};

const DOT_CLASSES: Record<string, string> = {
  connecting: "bg-warn animate-pulse",
  connected: "bg-ok",
  closed: "bg-subtle",
  error: "bg-danger",
};

export function ConnectionIndicator(): ReactNode {
  const { status, nodeId, serverVersion, error, reconnect } = useCluster();

  // The node id + version come from the ServerHello, so they name the replica
  // actually serving this stream -- the thing you need when a two-replica
  // mesh behaves differently on one of them.
  const detail =
    status === "connected"
      ? [nodeId, serverVersion].filter(Boolean).join(" · ")
      : error;

  return (
    <div className="flex items-center gap-2 text-sm">
      <span
        aria-hidden="true"
        className={`inline-block h-2 w-2 rounded-full ${DOT_CLASSES[status] ?? "bg-subtle"}`}
      />
      <span role="status" className="text-fg">
        {LABELS[status] ?? status}
      </span>
      {detail ? (
        <span className="max-w-80 truncate font-mono text-xs text-muted" title={detail}>
          {detail}
        </span>
      ) : null}
      {status === "error" || status === "closed" ? (
        <button
          type="button"
          onClick={reconnect}
          className="rounded border border-line px-2 py-0.5 text-xs text-fg hover:bg-raised"
        >
          Retry
        </button>
      ) : null}
    </div>
  );
}
