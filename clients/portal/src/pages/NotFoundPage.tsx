import { Link } from "react-router-dom";
import type { ReactNode } from "react";

// The client-side 404.
//
// It exists because the Go handler's SPA fallback means the SERVER cannot
// return 404 for an unknown route beneath /portal/ -- it has no way to tell a
// mistyped URL from a legitimate deep link the router knows about. So the
// router is the only place that can answer, and this is its answer.

export function NotFoundPage(): ReactNode {
  return (
    <section>
      <h1 className="text-xl font-semibold tracking-tight">Not found</h1>
      <p className="mt-1 text-sm text-muted">
        No portal page is routed at this address.
      </p>
      <Link to="/concepts" className="mt-4 inline-block text-sm text-accent hover:underline">
        Go to Concepts
      </Link>
    </section>
  );
}
