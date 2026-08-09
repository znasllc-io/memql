import type { ReactNode } from "react";
import { PROPORTION_BAR_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Elsewhere, Reading, Refused } from "./AdminLayout";
import { TOKEN_CONCEPT } from "./rows";
import { surfaceById } from "./urls";
import { useAdminAccess, useTokenConsole } from "./useAdminConsole";

// Sessions and tokens.
//
// WHAT "SESSIONS" MEANS HERE. The server-rendered console had a /admin/sessions
// route, and it was a placeholder that rendered a "tracked for a follow-up
// commit" notice -- there was never a session list to move. Open sessions ARE
// listed, in the People view, which reads v1:identity:authSession beside the
// people who own them. This page is the credential half: the long-lived tokens
// that outlive any session.
export function TokensPage(): ReactNode {
  const surface = surfaceById("tokens");
  const { role, canAdminister, resolved } = useAdminAccess();
  const console_ = useTokenConsole(canAdminister);

  if (surface === undefined) return null;
  if (!canAdminister) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} />
      </AdminFrame>
    );
  }

  const active = console_.tokens.filter((token) => token.state === "active").length;

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={<MetaButton onClick={console_.reload}>Refresh</MetaButton>}
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="Tokens issued"
            value={console_.loading ? "…" : String(console_.tokens.length)}
            sub="revoked ones included"
          />
          <Reading
            label="Still usable"
            value={console_.loading ? "…" : String(active)}
            sub="each one signs in as its owner"
          />
          <Reading
            label="People checked"
            value={console_.loading ? "…" : String(console_.scanned)}
            sub={
              console_.capped
                ? "the scan stopped at its ceiling"
                : "everyone who can sign in"
            }
          />
        </div>
        {console_.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read the tokens: {console_.error}</ErrorMessage>
          </div>
        )}
        {console_.capped ? (
          <p className="mt-3 text-xs text-subtle">
            The cluster publishes no query for every token at once, so this page
            reads them person by person and stops at its ceiling. A token held by
            someone further down the list is not shown.
          </p>
        ) : null}
      </Band>

      <Band title="By state" meta="Revoked rows stay listed so a revoke can be confirmed">
        {console_.tokens.length === 0 ? (
          <p className="text-sm text-subtle">
            {console_.loading ? "Reading tokens…" : "No personal access tokens have been issued."}
          </p>
        ) : (
          <ViewElement
            element={PROPORTION_BAR_ELEMENT}
            rows={console_.tokens}
            concept={TOKEN_CONCEPT}
            options={{ bindings: { value: [] } }}
          />
        )}
      </Band>

      <Band title="Every token" meta="Newest first" panel>
        {console_.tokens.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {console_.loading
              ? "Reading tokens…"
              : "Nothing to show. A person mints one of these from the CLI."}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={console_.tokens}
            concept={TOKEN_CONCEPT}
            options={{
              bindings: {
                column: ["owner", "label", "state", "lastUsedAt", "createdAt", "usableByAgents"],
              },
              sort: { field: "createdAt", direction: "desc" },
            }}
          />
        )}
      </Band>

      <Elsewhere what="Revoking a token">
        Revocation is not on this console. <code>revokePATIdentity</code> takes
        any identity id and applies no check of its own, so the owner-and-admin
        rule protecting it is the identity service's route rather than the
        cluster's — a button here would be gated by this page and nothing
        behind it. Revoke at <code>/admin/tokens</code> on the identity service,
        where that gate holds, until the mutation gates itself (memql#3324).
        Every revoke there writes a <code>pat_revoked_admin</code> audit event.
      </Elsewhere>

      <Elsewhere what="Node tokens">
        The machine credentials cluster nodes authenticate with are not listed
        here. Reading them means calling <code>nodeTokenIdentities</code>, which
        the cluster serves only to server-originated callers, and revoking one
        additionally has to run as a system credential actor — neither is
        something a browser can be. They stay on the identity service's own
        console at <code>/admin/tokens</code> (memql#3324).
      </Elsewhere>
    </AdminFrame>
  );
}
