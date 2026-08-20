import type { ReactNode } from "react";

// What a caller below owner/admin sees instead of the Modules surface --
// the AdminLayout.Refused shape, restated here because the sentence differs:
// this surface is about the cluster's operating state, and the engine
// refuses the reads themselves below admin (memql#4188), so the console
// offers nothing rather than a grid of permission errors.
export function ModulesRefused({ role, resolved }: { role: string; resolved: boolean }): ReactNode {
  return (
    <div className="rounded-lg border border-line bg-surface p-6">
      <h2 className="text-sm font-semibold">This is an owner and admin surface</h2>
      <p className="mt-2 max-w-2xl text-sm text-muted">
        {resolved
          ? `The cluster resolved your role on this connection as ${role === "" ? "unset" : role}.`
          : "Your role has not resolved on this connection yet."}{" "}
        The module inventory is the cluster's own operating state -- what runs, what is switched
        off, which environment variables are set -- and the engine refuses those reads below owner
        or admin. Ask a cluster owner to change your role.
      </p>
    </div>
  );
}
