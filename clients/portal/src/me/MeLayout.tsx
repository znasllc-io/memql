import { useState, type ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { Badge, Button, ConfirmDialog, Container, PageHeader, Skeleton, Tabs } from "../ui";
import { ME_FACETS, mePath } from "./urls";
import type { MeAccount } from "./useMe";

// The chrome the three profile facets share (memql#4318).
//
// # Tabs, and they are ROUTES
//
// Chosen over one column of bands (standard, and it sprawls as facets
// arrive) and a summary-card two-column layout (which would be the portal's
// first two-column page, for no reason but this one). Each tab is a real
// address -- /me, /me/sessions, /me/security -- the way /admin/* works, so a
// person can send somebody a link to their sessions list and a refresh lands
// where it left.
//
// # Sign out is the header's ONE primary action
//
// It is the page's only destructive verb and the reason a lot of people open
// a profile page at all, so it gets the single action slot rather than a row
// in the footer of a rail. `tone="danger"` and a ConfirmDialog: an accidental
// sign-out is cheap to recover from and annoying enough to be worth one
// keystroke of friction, and the confirm is what makes the same treatment
// honest next to the revokes on the Sessions tab.
//
// After signOut, RequireAuth renders the sign-in view IN PLACE -- no
// navigation, the URL stays -- which is what the portal already does
// everywhere else.

export function MeLayout({
  account,
  loading,
  children,
}: {
  account: MeAccount | null;
  loading: boolean;
  children: ReactNode;
}): ReactNode {
  const { signOut } = useAuth();
  const [confirming, setConfirming] = useState(false);

  const displayName = account?.displayName.trim() ?? "";
  const email = account?.primaryEmail.trim() ?? "";
  const role = account?.role ?? "";
  // The name if there is one, the email if there is not -- never the userId.
  const title = displayName === "" ? (email === "" ? "Your account" : email) : displayName;

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          title={loading && account === null ? <Skeleton variant="text" width="w-48" /> : title}
          blurb={
            account === null ? undefined : (
              <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                {displayName === "" || email === "" ? null : <span>{email}</span>}
                {role === "" ? null : <Badge>{role}</Badge>}
                {account.memberSince === "" ? null : (
                  <span className="text-subtle">
                    <span aria-hidden="true">· </span>member since {formatDay(account.memberSince)}
                  </span>
                )}
              </span>
            )
          }
          actions={
            <Button tone="danger" onClick={() => setConfirming(true)}>
              Sign out
            </Button>
          }
        />

        <div className="-mt-2">
          <Tabs
            label="Your account"
            items={ME_FACETS.map((facet) => ({
              to: mePath(facet.id),
              label: facet.label,
              end: true,
            }))}
          />
        </div>

        {children}

        <ConfirmDialog
          open={confirming}
          title="Sign out"
          confirmLabel="Sign out"
          tone="danger"
          onCancel={() => setConfirming(false)}
          onConfirm={() => {
            setConfirming(false);
            signOut();
          }}
        >
          This browser stops holding a credential for this cluster. Your other
          devices are untouched -- end those from the Sessions tab.
        </ConfirmDialog>
      </section>
    </Container>
  );
}

// formatDay renders a stored timestamp as a date a person reads, and renders
// anything it cannot parse verbatim rather than as "Invalid Date".
export function formatDay(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "--";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

// formatMoment keeps the time as well, for the columns where "when did this
// last happen" is the question and a date alone would collapse a whole day's
// activity into one answer.
export function formatMoment(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "--";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
