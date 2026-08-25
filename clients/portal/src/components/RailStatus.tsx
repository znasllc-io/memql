import type { ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { Button, StatusDot } from "../ui";
import { RefreshCw } from "../ui/icons";

// The rail footer: what this connection is, and nothing else (memql#4316).
//
// # What left, and why
//
// SidebarProfile carried six things -- connection dot, node id, version,
// email, role chip, Sign out -- with no divider above them, separated from
// the nav only by the parent's gap. Four of those were about the PERSON, and
// they now live where a person looks for themselves: the profile row at the
// top of the rail (RailProfileLink) and the page it opens. What is left is
// about the MACHINE, and a footer is the right place for it precisely
// because nobody needs to read it twice.
//
// The `border-t` is the change that makes it a footer rather than the last
// nav item. It was missing before, which is why the block read as one more
// thing in the list.
//
// # The facts
//
// GREEN IS THE TRANSPORT: `status === "connected"` is green, everything else
// is red. Connecting is not connected -- an amber "nearly" would let a console
// that is not reading anything look like one that is. RECONNECTING (memql#4537)
// is red for exactly that reason: the SDK will probably recover in a second,
// and until it does the console is not reading anything.
//
// THE NODE ID IS THE DOT'S LABEL. It names the replica serving THIS stream
// (ServerHello.node_id), which in a mesh is a different fact from "which
// cluster" -- that one is the address bar's job now.
//
// THE VERSION IS ServerHello.engine_version, the release the binary was cut
// from, and empty renders as "dev" -- the same answer core/buildinfo gives an
// uncut build. Not ServerHello.version, which is the wire protocol ("v1") and
// was rendered here as though it were the product.
//
// ONE role="status" IN THE WHOLE SHELL, and it is this one. A screen reader
// gets one live region for connection changes; a second would announce the
// same transition twice.

const LABELS: Record<string, string> = {
  idle: "Not connected",
  connecting: "Connecting",
  connected: "Connected",
  reconnecting: "Reconnecting",
  closed: "Disconnected",
  error: "Connection failed",
};

export function RailStatus({ collapsed }: { collapsed: boolean }): ReactNode {
  const { status, nodeId, engineVersion, error, reconnect, reconnectAttempt } = useCluster();

  const tone = status === "connected" ? "ok" : "danger";
  const versionLabel = engineVersion === "" ? "dev" : engineVersion;
  // The attempt count rides the label rather than a separate element: it is
  // the difference between "this blipped" and "this has been down a while",
  // which is the only question a person asks of a reconnecting indicator.
  const statusLabel =
    status === "reconnecting" && reconnectAttempt > 1
      ? `Reconnecting (${reconnectAttempt})`
      : (LABELS[status] ?? status);
  // Reconnecting is retryable too, and the button means something different
  // there: the SDK is already retrying on a backoff, so pressing it is
  // "sooner" rather than "instead" (memql#4537).
  const canRetry = status === "error" || status === "closed" || status === "reconnecting";

  // Collapsed, the node id has nowhere to render, so it joins the tooltip --
  // together with the status word and the error, which are the two other
  // things a person squinting at a red dot in an icon rail actually wants.
  const title = [statusLabel, nodeId, versionLabel, error].filter(Boolean).join(" · ");

  return (
    <div
      data-rail-status=""
      className={
        "border-t border-line pt-2 " +
        (collapsed
          ? "flex flex-col items-center gap-1.5 px-0.5"
          : "flex flex-col gap-1 px-1.5")
      }
      {...(collapsed ? { title } : {})}
    >
      <span role="status" className="sr-only">
        {statusLabel}
      </span>

      <div
        className={
          collapsed ? "flex flex-col items-center gap-1.5" : "flex min-w-0 items-center gap-1.5"
        }
      >
        <span data-connection-tone={tone} className="shrink-0">
          <StatusDot tone={tone} />
        </span>
        {collapsed || !nodeId ? null : (
          <span className="min-w-0 truncate font-mono text-xs text-fg" title={nodeId}>
            {nodeId}
          </span>
        )}
      </div>

      <span className="font-mono text-xs text-muted" title={versionLabel}>
        {versionLabel}
      </span>

      {collapsed || !error ? null : (
        <span className="truncate font-mono text-xs text-muted" title={error}>
          {error}
        </span>
      )}

      {canRetry ? (
        collapsed ? (
          <button
            type="button"
            onClick={reconnect}
            aria-label="Retry"
            title="Retry"
            className="rounded p-0.5 text-muted hover:bg-raised hover:text-fg"
          >
            <RefreshCw size={14} aria-hidden="true" />
          </button>
        ) : (
          <Button size="xs" onClick={reconnect}>
            Retry
          </Button>
        )
      ) : null}
    </div>
  );
}
