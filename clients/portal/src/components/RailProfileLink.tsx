import { NavLink } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { ME_ROOT } from "../me/urls";
import { Avatar, Badge, Skeleton } from "../ui";
import { navClass } from "./navRow";

// Who you are, at the top of the rail, linking to the page about you
// (memql#4317).
//
// # A nav row, not a card
//
// It takes the brand row's place, and it is styled as one of the rows below
// it -- same hover wash, same accent edge when active (navRow.ts holds the one
// recipe). A bordered card would give it more presence at the cost of being a
// box at the top of a rail that has no other boxes; a stacked header would be
// ~90px of chrome unlike anything else in the portal. It is a LINK, and rows
// are what links look like here.
//
// It sits BEFORE the first NavGroup and is not a group. The flat-nav ruling
// stands: one level of nesting for one destination buys nothing.
//
// # The identity comes from the stream, not the token
//
// useMyAccess asks the cluster who it resolved for THIS connection
// (userId / primaryEmail / clusterRole / displayName). The portal does not
// decode its own bearer, and the difference is the whole point: a rotated
// token, a revoked session or a role changed since the token was minted
// should show what the cluster is acting on, not what the browser believes.
//
// # It must never take the shell down
//
// This renders on every paint of the chrome, which is the same reason
// useSavedViews is try/catch-wrapped a few lines away in AppShell. useMyAccess
// swallows its own failures into `error` rather than throwing, so the row
// degrades to the skeleton and then to the email; and every field it reads
// goes through `text()` below rather than straight into `.trim()`, because
// AccessSummary is a WIRE shape and a type that promises a string is not the
// same as a payload that carries one.
//
// Three states, and each is a different fact:
//
//   unresolved     a Skeleton row. NOT the email-with-an-ellipsis the footer
//                  used to show: a name that is about to change reads as a
//                  name, and a person who glances at the wrong one has been
//                  told something false about which account they are in.
//   authDisabled   the "Authentication disabled" chip, in the row's place.
//                  There is no person to link to on a cluster that admits
//                  every dial as the synthetic local-dev owner, so a profile
//                  row would be a link to a fiction.
//   resolved       avatar, display name over email, role Badge.

// text is the one guard the row needs: a string field, trimmed, or "".
function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export function RailProfileLink({ collapsed }: { collapsed: boolean }): ReactNode {
  const { status: authStatus } = useAuth();
  const authDisabled = authStatus === "authDisabled";
  const { access, loading } = useMyAccess(!authDisabled);

  if (authDisabled) {
    return collapsed ? null : (
      <div className="px-1.5">
        <span
          className="inline-block rounded border border-warn bg-warn-subtle px-2 py-0.5 text-xs text-fg"
          title="MEMQL_IDENTITY_ENABLED=false on this cluster: every connection is admitted as the local-dev cluster owner. Never the case on a cluster anyone else can reach."
        >
          Authentication disabled
        </span>
      </div>
    );
  }

  if (access === null) {
    // `loading` is not the condition -- `access === null` is. The row has
    // nothing to render whether a read is in flight or one failed, and
    // showing a half-row for the second case would claim the connection
    // resolved somebody.
    return (
      <div data-profile-skeleton={loading ? "loading" : "unresolved"} className="px-1.5">
        <Skeleton variant="text" width={collapsed ? "w-6" : "w-40"} />
      </div>
    );
  }

  // READ DEFENSIVELY, and not only for the tests. AccessSummary is a WIRE
  // shape: display_name arrived in memql#4317, so a node that predates it
  // sends nothing and any hand-built summary (a stub, an older SDK) can be
  // missing a field the type promises. The rail renders on every paint of the
  // chrome -- a `.trim()` on undefined here takes the whole console down, not
  // just this row, which is the failure mode useSavedViews is try/catch'd for
  // a few lines away in AppShell.
  const displayName = text(access.displayName);
  const email = text(access.primaryEmail);
  const role = text(access.clusterRole);
  // The name if there is one, the email if there is not. Never the userId:
  // a canonical row id in the place a person's name goes reads as a bug.
  const primary = displayName === "" ? email : displayName;
  // Collapsed there is room for the avatar and nothing else, so all three
  // facts go to the tooltip AND the accessible name -- the second is what a
  // screen reader gets, and it must not be poorer than the hover.
  const facts = [primary, displayName === "" ? "" : email, role].filter(Boolean).join(" · ");

  return (
    <NavLink
      to={ME_ROOT}
      data-profile-row=""
      className={({ isActive }) => navClass(isActive, collapsed)}
      {...(collapsed ? { title: facts, "aria-label": facts } : {})}
    >
      <Avatar displayName={displayName} email={email} size={collapsed ? "sm" : "md"} />
      {collapsed ? null : (
        <span className="flex min-w-0 flex-col gap-0.5">
          <span className="truncate text-sm">{primary}</span>
          {displayName === "" || email === "" ? null : (
            <span className="truncate text-xs text-subtle">{email}</span>
          )}
          {role === "" ? null : (
            <span className="mt-0.5">
              <Badge>{role}</Badge>
            </span>
          )}
        </span>
      )}
    </NavLink>
  );
}
