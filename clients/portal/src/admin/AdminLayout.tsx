import type { ReactNode } from "react";

import { PageHeader, Tabs } from "../ui";
import { adminPath, ADMIN_SURFACES, type AdminSurface } from "./urls";

// The chrome the admin screens share.
//
// ===========================================================================
// WHY THIS IS NOT ViewFrame
// ===========================================================================
// The five predefined views are each ONE POPULATION read three ways, and their
// frame is built around that: an eyebrow naming the concept, a header meta slot
// reporting how much of the population has paged in. An admin screen is not a
// population. It is a question about the cluster's own operating state, and two
// of them render nothing that came out of the graph at all.
//
// So the grammar is kept and the frame is not: the header is the shared
// PageHeader, the surface strip is the shared Tabs idiom, and what changes is
// the one line where a view puts a concept id.
//
// ===========================================================================
// THE EYEBROW IS THE ROLE FLOOR
// ===========================================================================
// A view's eyebrow is the concept id -- the address of the rows, the single
// most useful fact about the page. The most useful fact about an admin page is
// a different one: WHO IS ALLOWED TO SEE IT, and that the permission is the
// cluster's rather than this console's. Every screen under /admin is owner or
// admin, enforced server-side on every read and every write, and an operator
// looking at an empty table should be able to tell "there is nothing here"
// from "you are not allowed to see it" without leaving the page.
//
// So the eyebrow states the floor and the console's actual answer for you. It
// is structure, not decoration: it is the only place the two facts appear
// together.

export function AdminFrame({
  surface,
  role,
  resolved,
  actions,
  children,
}: {
  surface: AdminSurface;
  role: string;
  resolved: boolean;
  // Right-aligned, prominent. Absent when the caller can do nothing here.
  actions?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <section className="flex min-h-full flex-col gap-6 pb-8">
      <PageHeader
        eyebrow={
          <>
            owner or admin
            <span aria-hidden="true"> · </span>
            {resolved
              ? role === ""
                ? "no role on this connection"
                : `you are ${role}`
              : "resolving your role"}
          </>
        }
        title={surface.title}
        blurb={surface.blurb}
        {...(actions === undefined ? {} : { actions })}
      />

      {/* The surfaces, as the one Tabs idiom directly under the header. A
          second nav RAIL would compete with the shell's; a tab strip reads as
          what it is -- facets of one console, not unrelated destinations. */}
      <div className="-mt-2">
        <Tabs
          label="Administration"
          items={ADMIN_SURFACES.map((s) => ({
            to: adminPath(s.id),
            label: s.label,
            end: true,
          }))}
        />
      </div>

      {children}
    </section>
  );
}

// Refused is what a caller below the floor sees instead of a page.
//
// It says the role it read and where the decision was made, because the two
// remedies are different: a wrong role is an owner's job, and a role that
// never resolved is a connection problem.
export function Refused({ role, resolved }: { role: string; resolved: boolean }): ReactNode {
  return (
    <div className="rounded-lg border border-line bg-surface p-6">
      <h2 className="text-sm font-semibold">This is an owner and admin surface</h2>
      <p className="mt-2 max-w-2xl text-sm text-muted">
        {resolved
          ? `The cluster resolved your role on this connection as ${role === "" ? "unset" : role}.`
          : "Your role has not resolved on this connection yet."}{" "}
        Every read behind these screens is refused server-side for anyone below
        owner or admin, so the console offers nothing here rather than showing
        you tables that would come back empty. Ask a cluster owner to change your
        role.
      </p>
    </div>
  );
}

// Elsewhere names a capability this console does NOT have, and where it lives.
//
// ===========================================================================
// WHY A CONSOLE ADVERTISES WHAT IT CANNOT DO
// ===========================================================================
// The usual rule is to hide an action the caller cannot take, and every other
// surface in this portal follows it. This is the deliberate exception, because
// the thing being hidden here is not a PERMISSION but a MISSING SEAM: rotating
// a signing key, editing a setting, revoking a node token and changing a
// person's role are all things an owner may absolutely do -- just not from a
// browser, because the cluster publishes no client-reachable call for them
// (memql#3324; the reasons are enumerated in wire.ts).
//
// An owner who cannot find "Rotate now" concludes the console is broken, or
// worse, that the capability is gone. Saying where it went is the difference
// between a migration in progress and a regression.
export function Elsewhere({
  what,
  children,
}: {
  what: string;
  children: ReactNode;
}): ReactNode {
  return (
    <div className="rounded-lg border border-line border-dashed bg-surface p-4">
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">{what}</h3>
      <p className="mt-2 max-w-3xl text-sm text-muted">{children}</p>
    </div>
  );
}

// A short, full-contrast reading beside a label. Used for the header numbers
// that are not a row set -- a count of keys, the last rotation -- where a stat
// tile over a fabricated row array would be a lie about where the number came
// from.
export function Reading({
  label,
  value,
  sub,
}: {
  label: string;
  value: string;
  sub?: string;
}): ReactNode {
  return (
    <div className="min-w-32 rounded-lg border border-line bg-surface px-3 py-2">
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-0.5 text-base font-semibold break-all">{value}</div>
      {sub === undefined ? null : <div className="mt-0.5 text-xs text-subtle">{sub}</div>}
    </div>
  );
}
