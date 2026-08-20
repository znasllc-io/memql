import type { ReactNode } from "react";

// The two non-content TEXT states a data pane can need: failed and
// nothing-to-show. Loading is not here any more -- a loading surface renders
// a shaped ui/Skeleton so layout never jumps when data lands (memql#4180). One component so they read identically
// wherever they appear -- an operator should not have to work out whether an
// empty pane means "no rows" or "the request died".

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
