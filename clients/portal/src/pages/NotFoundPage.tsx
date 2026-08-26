import { Link } from "react-router-dom";
import type { ReactNode } from "react";

import { Constellation } from "../ui";

// The client-side 404.
//
// It exists because the Go handler's SPA fallback means the SERVER cannot
// return 404 for an unknown route beneath the portal's origin -- it has no way
// to tell a mistyped URL from a legitimate deep link the router knows about.
// So the router is the only place that can answer, and this is its answer.
//
// It carries the Constellation (decision D4) because it is one of the four
// surfaces where a person is looking at a screen with nothing on it. The mark
// is what makes the page read as part of the product rather than as the
// browser's own error.

export function NotFoundPage(): ReactNode {
  return (
    <section className="flex flex-col items-center gap-4 py-16 text-center">
      <span className="text-accent opacity-80">
        <Constellation size="md" />
      </span>
      <h1 className="text-xl font-semibold tracking-tight">Not found</h1>
      <p className="max-w-md text-sm text-muted">
        Nothing is at this address. Check the link, or press Cmd+K to search
        everywhere you can go.
      </p>
      <Link to="/" className="text-sm text-accent hover:underline">
        Back to the console
      </Link>
    </section>
  );
}
