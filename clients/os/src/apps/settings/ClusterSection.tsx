import { Button, Caption } from "../../kit";
import { useSession } from "../../chrome/access";
import { useOsConnection } from "../../live/connection";
import { resolvedVersion, ridesTheSpine } from "./clusterRows";
import { mailHeadline, mailTone } from "./mailStatus";
import {
  useClusterIdentity,
  useDeploymentFacts,
  useMailStatus,
  useProviderStatus,
} from "./useClusterFacts";

// Cluster facts (memql#4742) -- READ-ONLY. No control on this surface
// mutates anything: the two buttons are Refresh, and Refresh re-asks a
// question. Cluster settings editing stays in the portal's admin console
// and deploy control stays with the portal and the cockpit.
//
// NO SECRET APPEARS HERE, and that is a property of the surfaces consumed
// rather than of this file's discretion: `integrationStatus` reports slot
// PRESENCE and never a value (its redactSecrets invariant), and
// `providerAuthStatus` carries no credential or fingerprint of one. The
// panel adds no secret-bearing read of its own.
//
// WHAT THIS PANEL DELIBERATELY DOES NOT CLAIM. The epic's brief named
// database engine facts and a "JWKS reachable" line off
// `v1:cluster:identityProvider.status` + `lastVerifiedAt`. Those fields have
// no writer: `createDatabase` stamps `engine`/`status` and never
// `engineVersion`/`extensions`, `createIdentityProvider` never writes
// `jwksUrl` or `lastVerifiedAt`, and `status` is stamped "connected" at
// bootstrap and never refreshed. Rendering a frozen literal as a health
// check is worse than omitting it -- an operator would read "JWKS
// reachable" from a constant. The engine version shown here is the LIVE one
// the node reports on the ServerHello, and the deployment pins come from
// rows something actually writes. See memql#4742 for the bootstrap-path
// work that would make the rest real.

export function ClusterSection() {
  const { access, config } = useSession();
  const connection = useOsConnection();
  const identity = useClusterIdentity();
  const deployment = useDeploymentFacts(identity.cluster?.id ?? "");
  const mail = useMailStatus(true);
  const providers = useProviderStatus(true);

  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Cluster</h3>

      <section className="os-field-group" aria-label="Cluster and identity">
        <h4 className="os-subhead">Cluster and identity</h4>
        <dl className="os-facts">
          <dt>Domain</dt>
          <dd className="os-mono">{config.domain || "unknown"}</dd>
          <dt>Identity issuer</dt>
          <dd className="os-mono">{config.identityUrl || "unknown"}</dd>
          <dt>Name</dt>
          <dd>{identity.cluster?.name || dash(identity.loading)}</dd>
          <dt>Region</dt>
          <dd>{identity.cluster?.region || dash(identity.loading)}</dd>
          <dt>Status</dt>
          <dd>{identity.cluster?.status || dash(identity.loading)}</dd>
          <dt>Deploy target</dt>
          <dd>{identity.cluster?.provider || dash(identity.loading)}</dd>
        </dl>
        <Caption>
          The domain and issuer are the values this cluster served to this
          client, not a re-derivation -- one derivation, and a second copy
          would disagree.
        </Caption>
      </section>

      <section className="os-field-group" aria-label="Versions">
        <h4 className="os-subhead">Versions</h4>
        <dl className="os-facts">
          <dt>Engine (this node)</dt>
          <dd className="os-mono">{connection?.engineVersion || "unknown"}</dd>
          <dt>Engine commit</dt>
          <dd className="os-mono">{connection?.engineCommit || "unknown"}</dd>
          <dt>Answered by</dt>
          <dd className="os-mono">{connection?.nodeId || "unknown"}</dd>
        </dl>
        {deployment.latest === null ? (
          <Caption>
            {deployment.loading
              ? "Loading from the cluster"
              : "No deployment has been recorded for this cluster."}
          </Caption>
        ) : (
          <>
            <dl className="os-facts">
              <dt>Deployment</dt>
              <dd className="os-mono">{deployment.latest.deploymentId}</dd>
              <dt>Deployment status</dt>
              <dd>{deployment.latest.status}</dd>
              <dt>Deployment engine version</dt>
              <dd className="os-mono">{deployment.latest.version || "unknown"}</dd>
            </dl>
            {deployment.specs.length === 0 ? (
              <Caption>No per-node pins recorded for this deployment.</Caption>
            ) : (
              <ul className="os-hidden-list" aria-label="Node type versions">
                {deployment.specs.map((spec) => (
                  <li key={spec.nodeType}>
                    <span className="os-mono">{spec.nodeType}</span>{" "}
                    {resolvedVersion(spec, deployment.latest?.version ?? "") || "unknown"}
                    {ridesTheSpine(spec) ? " (engine version)" : " (pinned)"} &times;{" "}
                    {spec.replicas}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
        {deployment.error ? <Caption>{deployment.error}</Caption> : null}
      </section>

      <section className="os-field-group" aria-label="Mail sender">
        <h4 className="os-subhead">Mail sender</h4>
        {mail.error ? (
          <Refusal role={access?.clusterRole ?? ""} message={mail.error} />
        ) : mail.value === null ? (
          <Caption>{mail.loading ? "Loading from the cluster" : "No mail status reported."}</Caption>
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
            <dl className="os-facts">
              <dt>Configured</dt>
              <dd>{mail.value.configured}</dd>
              <dt>Health</dt>
              <dd>{mail.value.health}</dd>
            </dl>
            {mail.value.detail ? <Caption>{mail.value.detail}</Caption> : null}
          </>
        )}
        <Refresh facts={mail} serverStamp={mail.value?.checkedAt ?? ""} />
      </section>

      <section className="os-field-group" aria-label="AI providers">
        <h4 className="os-subhead">AI providers</h4>
        {providers.error ? (
          <Refusal role={access?.clusterRole ?? ""} message={providers.error} />
        ) : providers.value === null ? (
          <Caption>
            {providers.loading ? "Loading from the cluster" : "No providers reported."}
          </Caption>
        ) : providers.value.length === 0 ? (
          <Caption>
            No AI providers are configured. That is how a freshly installed
            cluster starts.
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="AI providers">
            {providers.value.map((p) => (
              <li key={p.name}>
                <span
                  className="os-dot"
                  data-os-dot={p.available ? "reachable" : "unreachable"}
                  role="img"
                  aria-label={p.available ? "available" : "unavailable"}
                />{" "}
                <span className="os-mono">{p.name}</span> -- {p.vendor} {p.model}, credential
                from {p.authSource || "unknown"}
                {p.reason ? ` -- ${p.reason}` : ""}
              </li>
            ))}
          </ul>
        )}
        <Refresh facts={providers} serverStamp="" />
      </section>
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
      <Button onClick={facts.reload} busy={facts.loading} busyLabel="Reading">
        Refresh
      </Button>
      <Caption>
        {stamp ? `Read at ${stamp}. ` : ""}A projection of one node's own
        registry -- which replica answered is not knowable from here.
      </Caption>
    </div>
  );
}

function dash(loading: boolean): string {
  return loading ? "Loading from the cluster" : "--";
}
