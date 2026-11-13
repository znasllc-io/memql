import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { ContentSkeleton, InlineSkeleton } from "../../kit/ContentSkeleton";
import { AutoRefresh, Details, DetailsSection, Fact, Facts, Head, RecordList, RecordRow, Subhead } from "../../kit";
import { Caption } from "../../kit";
import { useSession } from "../../chrome/access";
import { useOsConnection } from "../../live/connection";
import { resolvedVersion, ridesTheSpine } from "./clusterRows";
import { mailHeadline, mailTone } from "./mailStatus";
import {
  useClusterIdentity,
  useDeploymentFacts,
  useInfrastructureFacts,
  useMailStatus,
} from "./useClusterFacts";

// Read-only cluster facts now live in Cluster > Overview. The sign-in policy
// and brand form live in Identity; mounting facts must never read that form.
// No secret appears here: integrationStatus reports presence, never values.
//
// WHAT THIS PANEL CLAIMS, AND WHAT IT STILL WILL NOT.
//
// The epic's brief named database engine facts and a "JWKS reachable" line.
// The first half is now real and rendered: memql#4766 gave `engineVersion`,
// `extensions`, `extensionVersions`, `jwksUrl` and `acceptedAudiences` actual
// writers -- the database facts are probed from the live connection at
// startup, and the two identity fields were always computed and are now
// forwarded rather than dropped.
//
// The health line is not, and will not be. `identityProvider.status` and
// `lastVerifiedAt` were REMOVED rather than filled: no writer is honest at
// that granularity (every node refreshes JWKS on a five-minute timer, so
// either every node writes or the row reports one node's view), and a stored
// freshness stamp read an hour later is not a health signal.
// `database.status` was removed as structurally unanswerable -- the row lives
// in the database it describes, so a successful read can only ever say
// "healthy". Rendering a frozen literal as a health check is worse than
// omitting it. Do not add either line back off these rows; probe live and say
// when you looked.
//
// The engine version in the connection block is still the LIVE one off the
// ServerHello. It sits beside the recorded one on purpose: they answer
// different questions -- what this node is talking to now, versus what the
// cluster recorded at its last bff start -- and a disagreement between them
// is worth seeing.

export function ClusterSection({ inline = false }: { inline?: boolean }) {
  const { access, config } = useSession();
  const connection = useOsConnection();
  const identity = useClusterIdentity();
  const deployment = useDeploymentFacts(identity.cluster?.id ?? "");
  const mail = useMailStatus(true);
  // Owner-only reads, same as providerAuthStatus: gate the CALL on the role so
  // an admin does not issue a read whose empty answer we already know, and let
  // the engine remain the authority either way.
  const infra = useInfrastructureFacts(access?.role === "owner");

  return (
    <div className="os-cluster">
      {inline ? null : <Head title="Cluster facts" />}
      <Details>

        <DetailsSection title="Cluster and identity">
          <Facts>
            <Fact label="Domain" mono value={<>{config.domain || "unknown"}</>} />
            <Fact label="Identity issuer" mono value={<>{config.identityUrl || "unknown"}</>} />
            <Fact label="Name" value={<>{identity.cluster?.name || dash(identity.loading)}</>} />
            <Fact label="Region" value={<>{identity.cluster?.region || dash(identity.loading)}</>} />
            {/* NO "Status" ROW. `v1:cluster:cluster.status` is gone (memql#4772):
                its only writer stamped a constant at bootstrap and nothing ever
                refreshed it, so this line said "healthy" with every node down --
                and it said it HERE, on the panel, as well as in the diagnostics
                report. A health verdict for this cluster has to be DERIVED at
                read time (seedNodeTypes against live v1:cluster:node rows), not
                read from a stored field. */}
            <Fact label="Deploy target" value={<>{identity.cluster?.provider || dash(identity.loading)}</>} />
          </Facts>
          <Caption>
            The domain and issuer are the values this cluster served to this
            client, not a re-derivation -- one derivation, and a second copy
            would disagree.
          </Caption>
        </DetailsSection>

        <DetailsSection title="Versions">
          <Facts>
            <Fact label="Engine (this node)" mono value={<>{connection?.engineVersion || "unknown"}</>} />
            <Fact label="Engine commit" mono value={<>{connection?.engineCommit || "unknown"}</>} />
            <Fact label="Answered by" mono value={<>{connection?.nodeId || "unknown"}</>} />
          </Facts>
          {deployment.latest === null ? (deployment.loading ? <RecordListSkeleton label="Loading from the cluster" /> : <Caption>{"No deployment has been recorded for this cluster."}</Caption>) : (
            <>
              <Facts>
                <Fact label="Deployment" mono value={<>{deployment.latest.deploymentId}</>} />
                <Fact label="Deployment status" value={<>{deployment.latest.status}</>} />
                <Fact label="Deployment engine version" mono value={<>{deployment.latest.version || "unknown"}</>} />
              </Facts>
              {deployment.specs.length === 0 ? (
                <Caption>No per-node pins recorded for this deployment.</Caption>
              ) : (
                <><Subhead meta={!deployment.loading && !deployment.error ? deployment.specs.length : undefined}>Node type versions</Subhead>
                <RecordList as="ul" label="Node type versions">{deployment.specs.map(spec => <RecordRow key={spec.nodeType} name={spec.nodeType} secondary={resolvedVersion(spec, deployment.latest?.version ?? "") || "unknown"} state={ridesTheSpine(spec) ? "engine version" : "pinned"}><span>{spec.replicas} replicas</span></RecordRow>)}</RecordList></>
              )}
            </>
          )}
          {deployment.error ? <Caption>{deployment.error}</Caption> : null}
        </DetailsSection>

        <DetailsSection title="Infrastructure">
          {access?.role !== "owner" ? (
            <Caption>
              The database and identity-provider records are cluster-owner only. This section admits
              admins, and the engine decides that read, not this window.
            </Caption>
          ) : infra.error ? (
            <Refusal role={access?.role ?? ""} message={infra.error} />
          ) : infra.loading ? (
            <ContentSkeleton kind="form" label="Loading from the cluster" />
          ) : (
            <>
              {infra.database === null ? (
                <Caption>
                  No database record. It is written when a bff node starts, so a cluster that has not
                  restarted since this was added will not have one yet.
                </Caption>
              ) : (
                <Facts>
                  <Fact label="Database" mono value={<>{infra.database.dbName} on {infra.database.host}:{infra.database.port}</>} />
                  <Fact label="Recorded engine" mono value={<>{infra.database.engine} {infra.database.engineVersion || "--"}</>} />
                  <Fact label="SSL mode" mono value={<>{infra.database.sslMode || "--"}</>} />
                  <Fact label="Extensions" mono value={<>{infra.database.extensions.length === 0
                      ? "--"
                      : infra.database.extensions
                          .map((name) => {
                            const version = infra.database?.extensionVersions[name];
                            return version ? `${name} ${version}` : name;
                          })
                          .join(", ")}</>} />
                </Facts>
              )}
              {infra.identityProvider === null ? null : (
                <Facts>
                  <Fact label="Identity provider" mono value={<>{infra.identityProvider.name} ({infra.identityProvider.providerType})</>} />
                  <Fact label="Issuer" mono value={<>{infra.identityProvider.issuerUrl || "--"}</>} />
                  <Fact label="JWKS" mono value={<>{infra.identityProvider.jwksUrl || "--"}</>} />
                  <Fact label="Accepted audiences" mono value={<>{infra.identityProvider.acceptedAudiences.length === 0
                      ? "--"
                      : infra.identityProvider.acceptedAudiences.join(", ")}</>} />
                </Facts>
              )}
              <Caption>
                Recorded when a bff node last started -- not a live probe, and deliberately carrying
                no health verdict.
              </Caption>
            </>
          )}
        </DetailsSection>

        <DetailsSection title="Mail sender">
          {mail.error ? (
            <Refusal role={access?.role ?? ""} message={mail.error} />
          ) : mail.value === null ? (
            mail.loading ? <ContentSkeleton kind="detail" label="Loading mail status" /> : <Caption>No mail status reported.</Caption>
          ) : (
            <>
              <p className="os-stub-summary">
                <span
                  className="os-dot"
                  data-os-dot={mailTone(mail.value)}
                  role="img"
                  aria-label={`Mail health: ${mail.value.health}`}
                />{" "}
                {mailHeadline(mail.value)}
              </p>
              <Facts>
                <Fact label="Configured" value={<>{mail.value.configured}</>} />
                <Fact label="Health" value={<>{mail.value.health}</>} />
              </Facts>
              {mail.value.detail ? <Caption>{mail.value.detail}</Caption> : null}
            </>
          )}
          <Refresh facts={mail} serverStamp={mail.value?.checkedAt ?? ""} />
        </DetailsSection>

      </Details>
    </div>
  );
}

/**
 * A refusal renders WHERE THE PANEL WOULD BE, in the engine's own words.
 * `providerAuthStatus` is owner-only while this section admits admin, so an
 * admin seeing this here is the system working -- the role floor for the
 * section and the floor for one read inside it are genuinely different, and
 * flattening them would either hide the section from admins or promise them
 * a panel that always fails.
 */
function Refusal({ role, message }: { role: string; message: string }) {
  return (
    <>
      <p className="os-stub-summary">
        The cluster declined this read for {role || "your role"}.
      </p>
      <Caption>{message}</Caption>
    </>
  );
}

/**
 * `serverStamp` wins when the reply carries one: `integrationStatus` stamps
 * `checkedAt` in the handler, and the moment the NODE looked is the honest
 * answer. The client clock only says when the reply arrived here.
 */
function Refresh({
  facts,
  serverStamp,
}: {
  facts: { loading: boolean; fetchedAt: number | null; reload: () => void };
  serverStamp: string;
}) {
  const stamp = serverStamp || (facts.fetchedAt === null ? "" : new Date(facts.fetchedAt).toISOString());
  return (
    <div className="os-refresh-row">
      <AutoRefresh onRefresh={facts.reload} busy={facts.loading} />
      <Caption>
        {stamp ? `Read at ${stamp}. ` : ""}A projection of one node's own
        registry -- which replica answered is not knowable from here.
      </Caption>
    </div>
  );
}

function dash(loading: boolean) {
  return loading ? <InlineSkeleton /> : "--";
}
