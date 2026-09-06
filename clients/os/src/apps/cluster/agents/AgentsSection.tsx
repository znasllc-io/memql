import { useCallback, useMemo, useState } from "react";
import { Bot } from "lucide-react";
import { Concepts, getRowByConceptAndId, type Connection, type LiveCollectionSpec, type Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Head,
  LiveList,
  Panel,
  Row as KitRow,
  Subhead,
  formatMoment,
  useLiveView,
} from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { useReading } from "../../../cluster/reading";
import {
  agentFingerprint,
  agentFromRow,
  authorizationFromRow,
  computerUseScopeReading,
  type AgentRow,
  type AuthorizationRow,
} from "./rows";

// Agents: every agent this cluster runs, live -- and the caller's own
// standing grants, which are a different question entirely.
//
// ===========================================================================
// THIS LIST IS PRESENTATION OVER AN UNGATED READ, AND THE UI MUST NOT IMPLY
// OTHERWISE
// ===========================================================================
// `v1:agents:agent` declares NO row-authz tier (dsl/agents/concepts.memql,
// `concept agent`, line 20 -- the sibling `agentAuthorization` two concepts
// down declares `@rowAuthz(owner="userId")`, so the absence here is not an
// oversight in the reading). Both `activeAgents` and `allAgents` therefore
// return EVERY agent in the cluster to any authenticated caller, including
// `systemPrompt` and `providerConfig`, which the `agentFull` shape projects.
//
// So this section's place behind the Cluster app's admin floor is a
// PRESENTATION choice about where the surface belongs, not a mirror of an
// engine gate. Nothing here is secured by being here. The page must not say
// or imply "only admins can see this" -- it would be false, and a false
// statement about who can read something is worse than no statement.
//
// ===========================================================================
// THE LIST IS GENUINELY LIVE
// ===========================================================================
// `graph.node.*.v1:agents:*` carries a broadcast routing rule
// (component/node/routing.go), so a subscription over this concept reaches a
// browser. That is checked rather than assumed: the first cut of Fleet
// reasoned from the ABSENCE of a rule with a concept's name in it and printed
// the mistake on the page.

/** How many grants the standing-authorizations panel shows before it stops
 *  listing. A caller's own grants are a small bounded set by construction --
 *  the query is `@unbounded` for exactly that reason -- so this is a guard
 *  against a pathological account, not a page size. */
const AUTHORIZATION_CAP = 50;

export function AgentsSection({ showInactive }: { showInactive: boolean }) {
  const spec = useCallback(
    (conn: Connection): LiveCollectionSpec<Row> => ({
      concept: Concepts.AGENTS_AGENT,
      seed: async (_cursor, signal) => {
        const result = showInactive
          ? await conn.query.allAgents({}, { signal })
          : await conn.query.activeAgents({}, { signal });
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, Concepts.AGENTS_AGENT, rowId, { signal });
        return (row as Row) ?? null;
      },
      paged: false,
    }),
    [showInactive],
  );

  // The key encodes WHICH READ this is. Flipping the preference changes the
  // seed, so it has to restart the collection rather than filter what the
  // previous seed happened to bring back.
  const feed = useLiveCollection<Row>(`cluster:agents:${showInactive}`, spec);

  const [openId, setOpenId] = useState("");

  const view = useLiveView<Row, AgentRow>(feed.source, `agents:${showInactive}`, (rows) =>
    rows.map(agentFromRow).filter((a) => a.id !== ""),
  );

  const open = useMemo(
    () => (view?.snapshot.rows ?? []).find((a) => a.id === openId) ?? null,
    [view, view?.snapshot, openId],
  );

  return (
    <div className="os-cluster">
      <Head title="Agents" meta={showInactive ? "every agent row" : "active agents"} />

      <div className="os-cluster-body">
        <div className="os-cluster-list">
          <LiveList<AgentRow>
            // Keyed on the filter so flipping the preference RE-BASELINES the
            // arrival cues: revealing rows this browser already had is not
            // the cluster sending them.
            key={`agents:${showInactive}`}
            source={view}
            rowId={(a) => a.id}
            fingerprint={agentFingerprint}
            label="Agents in this cluster"
            emptyText={
              showInactive
                ? "This cluster has no agent rows at all."
                : "No active agents. Turn on inactive agents in this app's settings if you are looking for one that was switched off."
            }
            renderRow={(agent) => (
              <KitRow
                icon={<Bot size={13} aria-hidden className="os-cluster-row-glyph" />}
                name={agent.name || agent.id}
                current={agent.active}
                dim={!agent.active}
                open={openId === agent.id}
                onOpen={() => setOpenId((held) => (held === agent.id ? "" : agent.id))}
                state={
                  <>
                    {agent.kind === "" ? null : <Chip tone="muted">{agent.kind}</Chip>}
                    {agent.active ? null : <Chip tone="accent">inactive</Chip>}
                  </>
                }
              >
                <span className="os-cluster-row-note">{agent.role || agent.roleSlug}</span>
              </KitRow>
            )}
          />
        </div>

        {open === null ? null : (
          <div className="os-cluster-detail">
            <Subhead>{open.name || open.id}</Subhead>
            <Facts>
              <Fact label="Kind" value={open.kind} />
              <Fact label="Role" value={open.role || open.roleSlug} />
              <Fact label="Owner" value={open.ownerUserId} mono />
              <Fact label="Active" value={open.active ? "yes" : "no"} />
            </Facts>
            {open.description === "" ? null : (
              <p className="os-cluster-fact">{open.description}</p>
            )}
            {open.capabilities.length === 0 ? (
              <Caption>This agent declares no capabilities.</Caption>
            ) : (
              <Chips label="Capabilities">
                {open.capabilities.map((c) => (
                  <Chip key={c} tone="muted">
                    {c}
                  </Chip>
                ))}
              </Chips>
            )}
            {open.groupIds.length === 0 ? null : (
              <Chips label="Groups">
                {open.groupIds.map((g) => (
                  <Chip key={g} tone="muted">
                    {g}
                  </Chip>
                ))}
              </Chips>
            )}
          </div>
        )}
      </div>

      <StandingAuthorizations />
    </div>
  );
}

function StandingAuthorizations() {
  const connection = useOsConnection();

  const read = useCallback(
    async (signal: AbortSignal): Promise<AuthorizationRow[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.agentAuthorizationsForSelf({}, { signal });
      return result.rows().slice(0, AUTHORIZATION_CAP).map(authorizationFromRow);
    },
    [connection],
  );

  const grants = useReading<AuthorizationRow[]>(
    "cluster:agents:authorizations",
    connection === null ? null : read,
  );

  const rows = grants.value ?? [];

  return (
    <Panel label="Your standing authorizations">
      <Subhead>Your standing authorizations</Subhead>
      {/* SELF-ONLY, SAID OUT LOUD. `v1:agents:agentAuthorization` declares
          @rowAuthz(owner="userId") and `agentAuthorizationsForSelf` filters
          userId==actor.userId, so this list is the CALLER's own grants and
          nobody else's -- including for a cluster owner, because the owner
          escape is not in that tier's read path. A panel of these under a
          cluster-wide heading would claim to show something it cannot, and
          an operator would read an empty list as "nobody has granted
          anything". */}
      <Caption>
        These are YOUR grants and nobody else's. There is no cluster-wide view of standing
        authorizations: the concept is owner-scoped, so even a cluster owner reads only their own.
      </Caption>

      {grants.state === "failed" ? (
        <p className="os-cluster-row-error os-mono">{grants.error}</p>
      ) : null}
      {grants.state === "reading" && grants.value === null ? (
        <Caption>Reading your grants.</Caption>
      ) : null}
      {grants.state === "read" && rows.length === 0 ? (
        <Caption>You have granted no agent a standing authorization.</Caption>
      ) : null}

      {rows.map((grant) => (
        <div key={grant.id} className="os-cluster-grant">
          <span className="os-cluster-grant-agent os-mono">{grant.agentId}</span>
          <Chips label={`Grant ${grant.id}`}>
            <Chip tone="muted" title="The plan kind this grant covers. * covers any kind.">
              {grant.planKind || "unstated"}
            </Chip>
            <Chip tone="muted" title="The space this grant is restricted to. * covers any space you own.">
              {grant.spaceScope || "unstated"}
            </Chip>
            <Chip
              tone={grant.computerUseScope === "" ? "muted" : "accent"}
              title="What this agent may do on your machines without asking again."
            >
              computer use: {computerUseScopeReading(grant.computerUseScope)}
            </Chip>
            {grant.active ? null : <Chip tone="muted">revoked</Chip>}
          </Chips>
          <Facts>
            <Fact
              label="Token cap"
              value={grant.tokenBudgetCap === null ? "your default budget" : grant.tokenBudgetCap.toLocaleString()}
              title="A grant with no cap uses your default plan token budget, which is a different statement from a cap of zero."
            />
            <Fact
              label="Expires"
              value={grant.expiresAt === "" ? "never" : formatMoment(grant.expiresAt)}
            />
          </Facts>
        </div>
      ))}
    </Panel>
  );
}
