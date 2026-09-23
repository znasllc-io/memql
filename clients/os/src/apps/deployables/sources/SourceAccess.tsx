import { useState } from "react";
import { Button, Caption, Select } from "../../../kit";
import { usePackageActions } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import type { PackageRow } from "../packages/rows";
import { bare } from "../people";
import { SwitchCredential } from "../page/stops/Source";
import { useSourceConnections } from "./connections";
import { returnPathFor } from "./connectReturn";
import { isGithubAppGrant, type CredentialRow } from "./rows";
import { sourceRecord } from "./sourceRecord";
import { useGithubConnect } from "./useGithubConnect";

export function SourceAccess({ pkg, credentials }: { pkg: PackageRow; credentials: readonly CredentialRow[] }) {
  const connections = useSourceConnections();
  const record = sourceRecord(pkg, credentials, connections.rows);
  const actions = usePackageActions();
  const reconnect = useGithubConnect();
  const [chosen, setChosen] = useState(pkg.sourceConnectionId || "");
  const mine = credentials.filter(row => bare(row.ownerUserId) === bare(pkg.ownerUserId) && !connections.revokedCredentialIds.includes(row.id));
  const options = connections.rows.filter(row => row.accountLogin.toLowerCase() === record.repository.split("/")[0]?.toLowerCase() && bare(row.ownerUserId) === bare(pkg.ownerUserId) && mine.some(grant => grant.id === row.credentialId && grant.status === "active"));
  const choice = options.find(row => row.id === chosen);
  const ready = connections.state === "live" && !connections.error;
  return <section className="os-report-part" aria-label="GitHub source access">
    <h4 className="os-report-heading">GitHub access</h4>
    {pkg.sourceConnectionId || options.length ? <>
      <fieldset className="deployable-mode-fieldset" disabled={actions.busy || !ready}><Select id={`source-path-${pkg.id}`} label="Account and organization" value={chosen} onChange={setChosen}>
        {!options.some(row => row.id === chosen) ? <option value={chosen}>{record.identity} · {record.target} (current)</option> : null}
        {options.map(row => <option key={row.id} value={row.id}>@{mine.find(grant => grant.id === row.credentialId)?.login || "unknown"} · {row.accountLogin}</option>)}
      </Select></fieldset>
      <div className="os-form-row"><Button tone="primary" busy={actions.busy} disabled={!ready || !choice || choice.id === pkg.sourceConnectionId} onClick={() => { if (choice) void actions.setCredential(pkg.id, choice.credentialId, choice.id); }}>Save access</Button></div>
      <Caption>The next fetch uses this account and installation. Existing deployables and their history stay intact.</Caption>
    </> : null}
    {!pkg.sourceConnectionId ? <SwitchCredential pkg={pkg} credentials={mine.filter(row => !isGithubAppGrant(row))} /> : null}
    {record.credential && isGithubAppGrant(record.credential) ? <div className="os-form-row"><Button busy={reconnect.busy} onClick={() => void reconnect.connect(returnPathFor("sources"), record.credential!.id)}>Reconnect {record.identity}</Button></div> : null}
    {connections.error ? <Caption>GitHub access could not be refreshed. Return to Sources and refresh to try again.</Caption> : null}
    {actions.refusal ? <ProblemNotice problem={actions.refusal} tone="error" /> : null}
    {reconnect.refusal ? <ProblemNotice problem={reconnect.refusal} tone="error" /> : null}
  </section>;
}
