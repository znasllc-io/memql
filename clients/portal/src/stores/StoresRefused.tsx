import type { ReactNode } from "react";

// What a non-owner sees instead of the Stores screen.
//
// v1:shopify:store is clusterOwner-tier: a store is one deployment's
// commerce configuration, not any operator's row, and "list every store"
// is not expressible at a weaker tier. The real gate is server-side -- every
// declared read carries actor.isClusterOwner==true as an explicit conjunct --
// so a caller below that tier gets an EMPTY result from the engine whatever
// this component renders. This exists so the empty result reads as "you may
// not see this" rather than "there are no stores".
export function StoresRefused({ role, resolved }: { role: string; resolved: boolean }): ReactNode {
  return (
    <div className="rounded-lg border border-line bg-surface p-6">
      <h2 className="text-sm font-semibold">This is a cluster-owner surface</h2>
      <p className="mt-2 max-w-2xl text-sm text-muted">
        {resolved
          ? `The cluster resolved your role on this connection as ${role === "" ? "unset" : role}.`
          : "Your role has not resolved on this connection yet."}{" "}
        A Shopify store carries the credentials a merchant's whole catalogue, customer list
        and order history are read with, so only the cluster owner may list, add or change
        one. Every read behind this screen is refused server-side for anyone else. Ask the
        cluster owner to make the change, or to change your role.
      </p>
    </div>
  );
}
