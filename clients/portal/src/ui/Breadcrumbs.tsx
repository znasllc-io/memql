import { Link } from "react-router-dom";
import type { ReactNode } from "react";

// Breadcrumbs, generalized from the concept browser's idiom: linked
// ancestors, plain-text current levels, slash separators. Standard on every
// detail page so "where am I, and how do I go up one level" has one answer
// everywhere.

export interface Crumb {
  label: string;
  // Linked when present; the current level and plain groupings omit it.
  to?: string;
}

export function Breadcrumbs({ items }: { items: readonly Crumb[] }): ReactNode {
  return (
    <nav aria-label="Breadcrumb" className="text-xs text-muted">
      {items.map((item, index) => (
        <span key={`${item.label}-${index}`}>
          {index > 0 ? <span className="mx-1.5 text-subtle">/</span> : null}
          {item.to === undefined ? (
            <span className="text-fg">{item.label}</span>
          ) : (
            <Link to={item.to} className="hover:text-fg hover:underline">
              {item.label}
            </Link>
          )}
        </span>
      ))}
    </nav>
  );
}
