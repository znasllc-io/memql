import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { Button, StatusDot } from "../ui";
import { LogOut, RefreshCw } from "../ui/icons";

// The rail footer's session block (memql#4240).
//
// Connection, engine version, identity, and sign-out used to share the
// topbar with the cluster host. That crammed four facts onto one row and
// rendered ServerHello.version ("v1", the wire protocol) as if it were the
// product. This block owns those facts, and the version it shows is
// ServerHello.engine_version -- empty (older node, or an uncut binary)
// renders as "dev", the same answer core/buildinfo gives.
//
// Green / red is the transport: useCluster().status === "connected" is
// green; everything else is red. Connecting is not connected. Retry for
// error / closed moves here from the old header indicator. RailMark at
// the top of the rail stays the ambient twin; this dot is the explicit
// owner-facing one.

const LABELS: Record<string, string> = {
  idle: "Not connected",
  connecting: "Connecting",
  connected: "Connected",
  closed: "Disconnected",
  error: "Connection failed",
};

export function SidebarProfile({ collapsed }: { collapsed: boolean }): ReactNode {
  const { status, nodeId, engineVersion, error, reconnect } = useCluster();
  const { status: authStatus, signOut } = useAuth();
  const { access, loading } = useMyAccess(authStatus !== "authDisabled");

  const connected = status === "connected";
  const tone = connected ? "ok" : "danger";
  const versionLabel = engineVersion === "" ? "dev" : engineVersion;
  const statusLabel = LABELS[status] ?? status;
  const canRetry = status === "error" || status === "closed";
  const email = access?.primaryEmail || access?.userId || (loading ? "…" : "Unknown");
  const role = access?.clusterRole ?? "";

  const title = collapsedTitle({
    statusLabel,
    nodeId,
    versionLabel,
    authDisabled: authStatus === "authDisabled",
    email,
    role,
    error,
  });

  return (
    <div
      className={
        collapsed
          ? "flex flex-col items-center gap-2 px-0.5 py-1"
          : "flex flex-col gap-1 px-1.5 py-1"
      }
      {...(collapsed ? { title } : {})}
    >
      <span role="status" className="sr-only">
        {statusLabel}
      </span>

      <div
        className={
          collapsed ? "flex flex-col items-center gap-2" : "flex min-w-0 items-center gap-1.5"
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

      {collapsed ? null : (
        <span className="font-mono text-xs text-muted" title={versionLabel}>
          {versionLabel}
        </span>
      )}

      {authStatus === "authDisabled" ? (
        collapsed ? null : (
          <span
            className="rounded border border-warn bg-warn-subtle px-2 py-0.5 text-xs text-fg"
            title="MEMQL_IDENTITY_ENABLED=false on this cluster: every connection is admitted as the local-dev cluster owner. Never the case in staging or production."
          >
            Authentication disabled
          </span>
        )
      ) : (
        <>
          {collapsed ? null : (
            <span className="min-w-0 truncate text-xs text-fg" title={identityTooltip(access)}>
              {email}
            </span>
          )}
          <div
            className={
              collapsed ? "flex flex-col items-center gap-2" : "flex flex-wrap items-center gap-1.5"
            }
          >
            {collapsed || !role ? null : (
              <span className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-muted">
                {role}
              </span>
            )}
            {collapsed ? (
              <button
                type="button"
                onClick={signOut}
                aria-label="Sign out"
                title="Sign out"
                className="rounded p-0.5 text-muted hover:bg-raised hover:text-fg"
              >
                <LogOut size={14} aria-hidden="true" />
              </button>
            ) : (
              <Button size="xs" onClick={signOut}>
                Sign out
              </Button>
            )}
          </div>
        </>
      )}

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

function identityTooltip(access: { userId: string; primaryEmail: string } | null): string {
  if (!access) return "";
  return [access.primaryEmail, access.userId].filter(Boolean).join(" · ");
}

function collapsedTitle(parts: {
  statusLabel: string;
  nodeId: string;
  versionLabel: string;
  authDisabled: boolean;
  email: string;
  role: string;
  error: string;
}): string {
  return [
    parts.statusLabel,
    parts.nodeId,
    parts.versionLabel,
    parts.authDisabled ? "Authentication disabled" : parts.email,
    parts.role,
    parts.error,
  ]
    .filter(Boolean)
    .join(" · ");
}
