import { useState, type ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { useAuth } from "../auth/AuthProvider";
import {
  Badge,
  Band,
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorNotice,
  Panel,
  Skeleton,
} from "../ui";
import { ViewElement } from "../views/ViewElement";
import { formatMoment } from "./MeLayout";
import { SESSION_CONCEPT, sessionTableRows } from "./rows";
import { useMySessions } from "./useMySessions";

// /me/sessions -- what can currently enter this account (memql#4319).
//
// # Why this list is worth a tab
//
// Until memql#4303 a browser session had no row at all, so a colleague who
// clicked your magic link first held a session that WAS NOT LISTED ANYWHERE
// and could not be revoked by anybody. The row exists now. This is where a
// person sees it, and the reason the page is not merely informational: every
// row here is a way in, and the point of showing them is that you can close
// one.
//
// # The table is view-kit's, and the revokes are their own band
//
// TABLE_ELEMENT draws the list, over an adapted row set (src/me/rows.ts) --
// the arrangement /admin/tokens uses for exactly this problem, a credential
// list with per-item revoke. The element supports one row action and it is
// hardcoded "View", so the destructive verb cannot live in the row; putting it
// in a following band is what TokensPage does and keeps one implementation
// drawing every table in the portal.
//
// # "This device" is a claim, not a guess
//
// It comes from MyAccessResult.session_id -- the row backing THIS connection,
// read by the server off the verified token. See useMySessions for why
// guessing by user agent would be actively dangerous.
//
// # Two verbs, and they say what they do
//
// Revoke ends one row. Sign out everywhere ends them ALL, this one included,
// because that is the call the engine has -- there is no everywhere-else
// message, and looping the single revoke over the other rows client-side would
// be a different operation wearing this one's name (a partial failure would
// leave a person believing they had closed a device they had not). So the
// button is labelled for what happens.
//
// Both confirm. The one that ends the session you are sitting in says so.

export function SessionsTab(): ReactNode {
  const { signOut } = useAuth();
  const sessions = useMySessions();
  const [pending, setPending] = useState<
    { kind: "one"; id: string; thisDevice: boolean } | { kind: "all" } | null
  >(null);

  async function confirm(): Promise<void> {
    if (pending === null) return;
    const target = pending;
    setPending(null);
    const signedOut =
      target.kind === "all" ? await sessions.revokeAll() : await sessions.revoke(target.id);
    // Revoking the row behind this connection leaves the portal holding a
    // bearer the cluster no longer honours. Dropping it locally is what puts
    // the person on the sign-in card instead of on a list whose next read
    // would be refused.
    if (signedOut) signOut();
  }

  const rows = sessionTableRows(sessions.sessions, formatMoment);
  const empty = !sessions.loading && sessions.error === "" && rows.length === 0;

  return (
    <div className="flex flex-col gap-6">
      {sessions.actionError === "" ? null : (
        <ErrorNotice
          sentence="That did not work."
          next="Nothing changed; try it again."
          detail={sessions.actionError}
        />
      )}

      {sessions.error !== "" ? (
        <ErrorNotice
          sentence="Could not read your sessions."
          next="Nothing is listed below. Do not read that as no other device being signed in -- this read failed, so the answer is unknown."
          detail={sessions.error}
        />
      ) : sessions.loading && rows.length === 0 ? (
        <Panel>
          <Skeleton variant="rows" rows={3} />
        </Panel>
      ) : empty ? (
        <EmptyState statement="No live sessions are recorded for this account. If you are reading this, your own credential carries no session row -- a personal access token or an operator key, rather than a browser sign-in." />
      ) : (
        <Band title="Live sessions" meta={`${rows.length} active`} panel>
          <ViewElement
            element={TABLE_ELEMENT}
            rows={rows}
            concept={SESSION_CONCEPT}
            options={{
              bindings: {
                column: ["device", "source", "signedIn", "lastActive", "thisDevice"],
              },
            }}
          />
        </Band>
      )}

      {rows.length === 0 ? null : (
        <Band title="End a session" meta="Takes effect on the next request that presents it">
          <ul className="flex flex-col gap-1">
            {sessions.sessions.map((session) => (
              <li
                key={session.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded border border-line bg-surface px-3 py-1.5"
              >
                <span className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                  <span className="min-w-0 break-all">{session.device}</span>
                  {session.thisDevice ? <Badge tone="ok">This device</Badge> : null}
                </span>
                <Button
                  size="xs"
                  tone="danger"
                  busy={sessions.busyId === session.id}
                  busyLabel="Revoking"
                  onClick={() =>
                    setPending({ kind: "one", id: session.id, thisDevice: session.thisDevice })
                  }
                >
                  Revoke
                </Button>
              </li>
            ))}
          </ul>
        </Band>
      )}

      <div>
        <Button
          tone="danger"
          busy={sessions.busyId === "all"}
          busyLabel="Signing out"
          disabled={rows.length === 0}
          onClick={() => setPending({ kind: "all" })}
        >
          Sign out everywhere
        </Button>
        <p className="mt-2 max-w-prose text-sm text-muted">
          Ends every session above, including this one. Use it when you think a session is
          somebody else&rsquo;s.
        </p>
      </div>

      <ConfirmDialog
        open={pending !== null}
        title={pending?.kind === "all" ? "Sign out everywhere" : "Revoke this session"}
        confirmLabel={pending?.kind === "all" ? "Sign out everywhere" : "Revoke"}
        tone="danger"
        onCancel={() => setPending(null)}
        onConfirm={() => void confirm()}
      >
        {pending?.kind === "all"
          ? "Every session on this account ends, this browser included. You will be signed out here and will need to sign in again."
          : pending?.thisDevice === true
            ? "This is the session you are using. Ending it signs you out here, and you will need to sign in again. Your other devices are untouched."
            : "That device stops being able to reach this account. Anyone using it will need to sign in again."}
      </ConfirmDialog>
    </div>
  );
}
