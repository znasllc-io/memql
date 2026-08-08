import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { useMyAccess } from "../cluster/useMyAccess";

// Who is signed in, and the ability to stop being.
//
// Sourced from the CLUSTER (useMyAccess -> MyAccessMsg), not from the token
// the portal holds -- see useMyAccess.ts for why the server's answer is the
// one worth rendering.
//
// Always present in the header, like the connection indicator next to it, and
// for the same reason: on an operations console the two questions "which
// cluster is this?" and "who am I acting as?" must both be answerable without
// clicking anything. An unlabelled console is how someone runs the right
// command against the wrong system.

export function IdentityBadge(): ReactNode {
  const { status, signOut } = useAuth();
  // Skipped when auth is disabled: this component renders the warning banner
  // in that mode and never the identity, so asking the cluster would be a
  // round trip whose answer is discarded.
  const { access, loading } = useMyAccess(status !== "authDisabled");

  if (status === "authDisabled") {
    // Said out loud, and in the warning colour. On a cluster running with
    // MEMQL_IDENTITY_ENABLED=false every stream is admitted as the synthetic
    // local-dev cluster owner -- an operator must not be able to mistake that
    // for having authenticated.
    return (
      <span
        className="rounded border border-warn bg-warn-subtle px-2 py-0.5 text-xs text-fg"
        title="MEMQL_IDENTITY_ENABLED=false on this cluster: every connection is admitted as the local-dev cluster owner. Never the case in staging or production."
      >
        Authentication disabled
      </span>
    );
  }

  const label = access?.primaryEmail || access?.userId || (loading ? "…" : "Unknown");

  return (
    <div className="flex items-center gap-2 text-sm">
      <span className="max-w-56 truncate text-fg" title={identityTooltip(access)}>
        {label}
      </span>
      {access?.clusterRole ? (
        <span className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-muted">
          {access.clusterRole}
        </span>
      ) : null}
      <button
        type="button"
        onClick={signOut}
        className="rounded border border-line px-2 py-0.5 text-xs text-fg hover:bg-raised"
      >
        Sign out
      </button>
    </div>
  );
}

function identityTooltip(access: { userId: string; primaryEmail: string } | null): string {
  if (!access) return "";
  // The user id belongs in the tooltip, not the header line: it is what an
  // operator needs when correlating with an audit row, and noise the rest of
  // the time.
  return [access.primaryEmail, access.userId].filter(Boolean).join(" · ");
}
