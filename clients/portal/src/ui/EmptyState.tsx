import type { ReactNode } from "react";

// An empty screen is an invitation to act, never a bare "No data". One plain
// statement of what is not here, and -- where an obvious next step exists --
// exactly one action with a verb for a label ("Create a site", "Invite
// someone"). No action slot filled means the emptiness is a fact the reader
// cannot change from here, and the statement alone is the honest render.

export function EmptyState({
  statement,
  action,
  icon,
}: {
  statement: ReactNode;
  action?: ReactNode;
  icon?: ReactNode;
}): ReactNode {
  return (
    <div className="flex flex-col items-center gap-3 rounded-lg border border-dashed border-line bg-surface px-6 py-10 text-center">
      {icon === undefined ? null : <div className="text-subtle">{icon}</div>}
      <p className="max-w-md text-sm text-muted">{statement}</p>
      {action === undefined ? null : <div>{action}</div>}
    </div>
  );
}
