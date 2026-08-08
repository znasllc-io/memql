import type { ReactNode } from "react";

// The three non-content states every data pane in the portal has: loading,
// failed, and nothing-to-show. One component so they read identically
// wherever they appear -- an operator should not have to work out whether an
// empty pane means "no rows" or "the request died".

export function Loading({ what }: { what: string }): ReactNode {
  return <p className="text-sm text-muted">Loading {what}…</p>;
}

export function Empty({ children }: { children: ReactNode }): ReactNode {
  return <p className="text-sm text-subtle">{children}</p>;
}

export function ErrorMessage({ children }: { children: ReactNode }): ReactNode {
  return (
    <p
      role="alert"
      className="rounded border border-danger bg-danger-subtle px-3 py-2 text-sm text-fg"
    >
      {children}
    </p>
  );
}
