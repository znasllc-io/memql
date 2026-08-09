import type { ReactNode } from "react";
import { PROPORTION_BAR_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Reading, Refused } from "./AdminLayout";
import { NODE_TOKEN_CONCEPT, TOKEN_CONCEPT } from "./rows";
import { surfaceById } from "./urls";
import {
  useAdminAccess,
  useAdminWrites,
  useNodeTokenConsole,
  useTokenConsole,
} from "./useAdminConsole";
import { WriteOutcome } from "./WriteOutcome";

// Sessions and tokens.
//
// WHAT "SESSIONS" MEANS HERE. The server-rendered console had a /admin/sessions
// route, and it was a placeholder that rendered a "tracked for a follow-up
// commit" notice -- there was never a session list to move. Open sessions ARE
// listed, in the People view, which reads v1:identity:authSession beside the
// people who own them. This page is the credential half: the long-lived tokens
// that outlive any session, in two families -- the personal access tokens
// people mint from the CLI, and the node_token credentials cluster nodes
// authenticate with.
//
// REVOKING IS THE POINT OF THE PAGE, and the button is not the gate. Both
// revokes go through IdentityAdminClient onto component/identity/adminops,
// which refuses below owner/admin server-side and audits the refusal. The node
// revoke additionally runs its engine write as the system credential actor,
// because a node_token row is a machine credential the memql#2513 guard admits
// only a system actor to -- that decision is the server's and is deliberately
// not expressible from here.
export function TokensPage(): ReactNode {
  const surface = surfaceById("tokens");
  const { role, canAdminister, resolved } = useAdminAccess();
  const console_ = useTokenConsole(canAdminister);
  const nodes = useNodeTokenConsole(canAdminister);
  const writes = useAdminWrites();

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
      actions={
        <MetaButton
          onClick={() => {
            console_.reload();
            nodes.reload();
          }}
        >
          Refresh
        </MetaButton>
      }
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
        <WriteOutcome state={writes} />
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

      <Band
        title="Revoke a personal access token"
        meta="Takes effect on the next call that presents it"
      >
        <RevokeList
          items={console_.tokens.map((token) => ({
            id: token.id,
            label: `${token.owner} — ${token.label}`,
            revoked: token.state === "revoked",
          }))}
          busy={writes.busy}
          empty="No personal access tokens have been issued."
          onRevoke={(id) =>
            writes.run(
              (client) => client.revokePersonalAccessToken(id),
              console_.reload,
            )
          }
        />
      </Band>

      <Band
        title="Node credentials"
        meta={`${nodes.tokens.length} minted, revoked ones included`}
        panel
      >
        {nodes.error !== "" ? (
          <ErrorMessage>Could not read the node tokens: {nodes.error}</ErrorMessage>
        ) : nodes.tokens.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {nodes.loading
              ? "Reading node credentials…"
              : "No node has bootstrapped a credential on this cluster."}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={nodes.tokens}
            concept={NODE_TOKEN_CONCEPT}
            options={{
              bindings: {
                column: ["node", "nodeType", "state", "lastConnectAt", "expiresAt", "mintedBy"],
              },
              sort: { field: "createdAt", direction: "desc" },
            }}
          />
        )}
      </Band>

      <Band
        title="Revoke a node credential"
        meta="The node cannot re-mint one afterwards"
      >
        <p className="mb-3 max-w-3xl text-xs text-subtle">
          Revoking flips the credential's active flag. The bootstrap gate
          consults it and refuses to re-mint, so the next time that node tries
          to self-bootstrap the identity service turns it away — an already
          issued JWT stops working when it expires or when the verifier's
          revocation gate catches it, whichever comes first.
        </p>
        <RevokeList
          items={nodes.tokens.map((token) => ({
            id: token.id,
            label: `${token.node} (${token.nodeType})`,
            revoked: token.state === "revoked",
          }))}
          busy={writes.busy}
          empty="Nothing to revoke."
          onRevoke={(id) =>
            writes.run((client) => client.revokeNodeToken(id), nodes.reload)
          }
        />
      </Band>

    </AdminFrame>
  );
}

// One revocable credential per row, with the row's own button.
//
// A LIST OF BUTTONS RATHER THAN A COLUMN IN THE TABLE ABOVE, deliberately. The
// tables here are rendered by view-kit's element library, which draws rows and
// does not host controls -- and it should not: an element that could embed an
// arbitrary action would stop being a rendering of data. So the destructive
// act lives in its own band, where it reads as a separate decision from
// reading the list, and where an already-revoked credential is visibly spent
// rather than offering a button that would do nothing.
function RevokeList({
  items,
  busy,
  empty,
  onRevoke,
}: {
  items: readonly { id: string; label: string; revoked: boolean }[];
  busy: boolean;
  empty: string;
  onRevoke: (id: string) => void;
}): ReactNode {
  if (items.length === 0) return <p className="text-sm text-subtle">{empty}</p>;
  return (
    <ul className="flex flex-col gap-1">
      {items.map((item) => (
        <li
          key={item.id}
          className="flex flex-wrap items-center justify-between gap-2 rounded border border-line bg-surface px-3 py-1.5"
        >
          <span className="min-w-0 text-sm">
            {item.label}
            <span className="ml-2 font-mono text-xs break-all text-subtle">{item.id}</span>
          </span>
          {item.revoked ? (
            <span className="text-xs text-subtle">already revoked</span>
          ) : (
            <MetaButton tone="danger" disabled={busy} onClick={() => onRevoke(item.id)}>
              Revoke
            </MetaButton>
          )}
        </li>
      ))}
    </ul>
  );
}
