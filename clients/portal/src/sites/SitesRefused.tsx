import type { ReactNode } from "react";

// What a non-owner sees instead of the sites screen.
//
// v1:platform:site is clusterOwner-tier, not owner-or-admin -- D6's own
// reasoning (dsl/platform/concepts.memql:site) is that "list every site in
// this cluster" and cluster-wide hostname uniqueness are not expressible at
// a weaker tier at all, so this deliberately does NOT reuse
// admin/AdminLayout.tsx's Refused (which states the owner-or-admin floor).
// Same shape and the same honesty, though: the real gate is server-side --
// both declared queries carry actor.isClusterOwner==true as an explicit
// conjunct, and a caller below that tier gets an EMPTY result from the
// engine regardless of what this component renders. This exists so that
// empty result reads as "you may not see this" rather than "there are no
// sites", which is the confusing-blank-state the issue calls out.
export function SitesRefused({ role, resolved }: { role: string; resolved: boolean }): ReactNode {
  return (
    <div className="rounded-lg border border-line bg-surface p-6">
      <h2 className="text-sm font-semibold">This is a cluster-owner surface</h2>
      <p className="mt-2 max-w-2xl text-sm text-muted">
        {resolved
          ? `The cluster resolved your role on this connection as ${role === "" ? "unset" : role}.`
          : "Your role has not resolved on this connection yet."}{" "}
        Sites are read and written under a stricter gate than owner-or-admin: only the
        cluster owner may list, create or change them. Every read behind this screen is
        refused server-side for anyone else, so the console offers nothing here rather than
        showing you a table that would come back empty. Ask the cluster owner to make the
        change, or to change your role.
      </p>
    </div>
  );
}
