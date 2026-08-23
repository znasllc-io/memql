import { useState, type ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { Badge, Band, Button, Callout, ConfirmDialog, DataText, EmptyState, Panel, Skeleton } from "../ui";
import { formatMoment } from "./MeLayout";
import { useMySessions } from "./useMySessions";

// /me/sessions -- what can currently enter this account (memql#4319).
//
// # Why this list is worth a tab
//
// Until memql#4303 a browser session had no row at all, so a colleague who
// clicked your magic link first held a session that WAS NOT LISTED ANYWHERE
// and could not be revoked by anybody. The row exists now. This is where a
// person sees it, and the reason the page is not just informational: every
// row here is a way in, and the point of showing them is that you can close
// one.
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
// message, and looping the single revoke over the other rows client-side
// would be a different operation wearing this one's name (a partial failure
// would leave a person believing they had closed a device they had not). So
// the button is labelled for what happens.
//
// Both confirm. The one that ends the session you are sitting in says so.

export function SessionsTab(): ReactNode {
  const { signOut } = useAuth();
  const sessions = useMySessions();
  const [pending, setPending] = useState<{ kind: "one"; id: string; thisDevice: boolean } | { kind: "all" } | null>(
    null,
  );

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

  return (
    <div className="flex flex-col gap-6">
      {sessions.actionError === "" ? null : (
        <Callout tone="danger" title="That did not work">
          {sessions.actionError}
        </Callout>
      )}

      {sessions.error !== "" ? (
        <Callout tone="danger" title="We could not read your sessions">
          {sessions.error} Nothing is shown below rather than an empty list -- an empty table here
          would read as &ldquo;no other device can reach your account&rdquo;, which is exactly the
          wrong thing to be reassured by.
        </Callout>
      ) : sessions.loading ? (
        <Panel>
          <Skeleton variant="rows" rows={3} />
        </Panel>
      ) : sessions.sessions.length === 0 ? (
        <EmptyState statement="No live sessions are recorded for this account. If you are reading this, your own credential carries no session row -- a personal access token or an operator key, rather than a browser sign-in." />
      ) : (
        <Band title="Live sessions" meta={`${sessions.sessions.length} active`} panel>
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs tracking-wide text-subtle uppercase">
                <th scope="col" className="px-3 py-2 font-semibold">
                  Device
                </th>
                <th scope="col" className="px-3 py-2 font-semibold">
                  Source
                </th>
                <th scope="col" className="px-3 py-2 font-semibold">
                  Signed in
                </th>
                <th scope="col" className="px-3 py-2 font-semibold">
                  Last active
                </th>
                <th scope="col" className="px-3 py-2 font-semibold">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {sessions.sessions.map((session) => (
                <tr key={session.id} className="border-t border-line align-top">
                  <td className="px-3 py-2">
                    <span className="flex flex-wrap items-center gap-2">
                      <span className="min-w-0 break-all">{session.device}</span>
                      {session.thisDevice ? <Badge tone="ok">This device</Badge> : null}
                    </span>
                  </td>
                  <td className="px-3 py-2">{session.source}</td>
                  <td className="px-3 py-2">
                    <DataText kind="time">{formatMoment(session.signedIn)}</DataText>
                  </td>
                  <td className="px-3 py-2">
                    <DataText kind="time">{formatMoment(session.lastActive)}</DataText>
                  </td>
                  <td className="px-3 py-2 text-right">
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
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Band>
      )}

      <div>
        <Button
          tone="danger"
          busy={sessions.busyId === "all"}
          busyLabel="Signing out"
          disabled={sessions.sessions.length === 0}
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
