import type { ReactNode } from "react";

// A surface whose route, nav entry and address exist before its body does.
//
// The portal's remaining surfaces (integration management, the view composer,
// the absorbed admin app) each land as their own change, but they all need the
// same three shared edits -- a route, a nav row, a path helper. Landing those
// edits once, up front, keeps three separate changes from each rewriting
// routes.tsx and AppShell.tsx and silently clobbering one another.
//
// Each surface owns a SPLAT route, so it adds its own sub-routes inside its own
// module without ever coming back to the route table. Replacing this
// placeholder is therefore the whole integration cost.
export function SurfacePlaceholder({
  title,
  blurb,
  issue,
}: {
  title: string;
  blurb: string;
  issue: string;
}): ReactNode {
  return (
    <section className="mx-auto max-w-2xl py-16 text-center">
      <h1 className="text-2xl font-semibold text-fg">{title}</h1>
      <p className="mt-3 text-sm text-muted">{blurb}</p>
      <p className="mt-6 text-xs text-subtle">
        This surface is reserved and not yet built ({issue}).
      </p>
    </section>
  );
}
