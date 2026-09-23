import { useEffect, useRef, useState } from "react";
import { GitBranch, Link2Off } from "lucide-react";
import { Button, Caption, Head, Notice, RecordList, RecordRow, RefreshButton, Subhead, listCount } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { AddButton } from "../../../kit/AddButton";
import { useSession } from "../../../chrome/access";
import { useCanManagePersonalSources } from "../parts";
import { bare } from "../people";
import { useWrite } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import { toneFor } from "../packages/refusals";
import { credentialIsRevoked, isGithubAppGrant, type CredentialFeedStatus, type CredentialRow } from "./rows";
import { createSourceConnection, removeSourceConnection, useSourceConnections, useSourceInstallations, type SourceConnectionRow } from "./connections";
import { DisconnectGitHub } from "./ConnectedAccountCard";
import { useCredentialRevoke, useGithubConnect } from "./useGithubConnect";
import { useGithubApp } from "./useGithubApp";
import { GithubAppMissing } from "./GithubAppSetup";
import { ConnectReturnNotice } from "./ConnectReturnNotice";
import { returnPathFor, type ConnectReturn } from "./connectReturn";
import { InstallLink } from "./RepositoryPicker";

export interface SourceConnectionsProps {
  mode: "choose" | "manage";
  selectedId?: string;
  onChoose?: (connection: SourceConnectionRow) => void;
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  can?: boolean;
  disabled?: boolean;
  connectResult?: ConnectReturn | null;
  returnSection?: string;
}
/** A source binds a personal GitHub identity to installation access.
 * Removing that binding never revokes its grant or changes deployables. */
export function SourceConnections(props: SourceConnectionsProps) {
  const { access } = useSession();
  const viewer = bare(access?.userId ?? "");
  const allowed = useCanManagePersonalSources();
  return <SourceConnectionsForViewer key={viewer} {...props} can={allowed && props.can !== false} viewer={viewer} />;
}
function SourceConnectionsForViewer({ mode, selectedId = "", onChoose, credentials, credentialFeed, can = true, disabled = false, connectResult, returnSection = "sources", viewer }: SourceConnectionsProps & { viewer: string }) {
  const feed = useSourceConnections();
  useEffect(() => { feed.observeCredentials(credentials); }, [credentials, feed.observeCredentials]);
  const [adding, setAdding] = useState(mode === "choose" && Boolean(connectResult));
  useEffect(() => { if (mode !== "choose") setAdding(false); else if (connectResult) setAdding(true); }, [mode, connectResult]);
  const creating = mode === "choose" && adding;
  const [removed, setRemoved] = useState<readonly string[]>([]);
  useEffect(() => {
    if (feed.state !== "live" || feed.error) return;
    // Once the authoritative read omits a removed binding, release the local
    // acknowledgement so a later re-add of that same tuple can appear again.
    setRemoved(held => held.some(id => !feed.rows.some(row => row.id === id)) ? held.filter(id => feed.rows.some(row => row.id === id)) : held);
  }, [feed.rows, feed.state, feed.error]);
  const mine = credentials.filter(grant => bare(grant.ownerUserId) === viewer && isGithubAppGrant(grant)).map(grant => feed.revokedCredentialIds.includes(grant.id) ? { ...grant, status: "revoked" } : grant);
  const rows = feed.rows.filter(row => !removed.includes(row.id));
  return <section aria-label="Source connections">
    <Head title="Sources" meta={listCount({ ...feed, rows })}>{mode === "choose" && can && !creating ? <AddButton label="Add source" disabled={disabled} onClick={() => setAdding(true)} /> : null}</Head>
    {mode === "manage" ? <Caption>Reconnect or remove sources here. Add new ones through Add deployable.</Caption> : null}
    <ConnectReturnNotice result={connectResult} />
    {creating ? <AddSource key={viewer} can={can && !disabled} initialCredentialId={connectResult?.credentialId} credentials={mine} credentialFeed={credentialFeed} returnSection={returnSection}
      onCancel={() => setAdding(false)} onSaved={() => { setAdding(false); setRemoved([]); feed.retry(); }} /> : <>
      {feed.error ? <Notice tone="error" sentence="Sources could not be read." detail={feed.error} /> : null}
      {rows.length === 0 ? <Caption>{feed.state === "seeding" ? "Reading sources…" : feed.state !== "live" || feed.error ? "Sources are unavailable. Refresh to try again." : mode === "manage" ? "No sources yet. Start with Add deployable." : "No sources yet. Add a GitHub account or organization to choose its repositories."}</Caption> : <RecordList as="ul" label="Sources">
        {rows.map(row => <SourceConnectionLine key={row.id} connection={row} grant={mine.find(grant => grant.id === row.credentialId)} mode={mode} selected={selectedId === row.id}
          onChoose={onChoose} can={can && !disabled} returnSection={returnSection} available={!disabled && feed.state === "live" && !feed.error && (!credentialFeed || credentialFeed.state === "live" && !credentialFeed.error)}
          onRemoved={() => { setRemoved(held => [...held, row.id]); feed.retry(); }} />)}
      </RecordList>}
      {feed.state !== "live" || feed.error ? <RefreshButton label="Refresh sources" busy={feed.state === "seeding"} onClick={feed.retry} /> : null}
      {mode === "manage" && mine.length > 0 ? <section className="os-field-group" aria-label="Connected GitHub accounts">
        <Subhead>GitHub accounts</Subhead>
        <RecordList as="ul" label="Connected GitHub accounts">{mine.map(grant => <GitHubAccount key={grant.id} grant={grant} returnSection={returnSection} can={can && !disabled} />)}</RecordList>
      </section> : null}
    </>}
  </section>;
}
function GitHubAccount({ grant, returnSection, can }: { grant: CredentialRow; returnSection: string; can: boolean }) {
  const disconnect = useCredentialRevoke();
  const feed = useSourceConnections();
  const reconnect = useGithubConnect();
  const previousStatus = useRef(grant.status);
  useEffect(() => {
    if (previousStatus.current === "revoked" && grant.status === "active") disconnect.clear();
    previousStatus.current = grant.status;
  }, [grant.status, disconnect.clear]);
  const revoked = credentialIsRevoked(grant) || disconnect.revokedCredentialId === grant.id;
  return <>
    <RecordRow name={`@${grant.login}`} secondary="GitHub account" state={revoked ? "Disconnected" : "Connected"}
      actions={can && revoked ? <Button busy={reconnect.busy} onClick={() => void reconnect.connect(returnPathFor(returnSection), grant.id)}>Reconnect GitHub</Button> : undefined} />
    {can && !revoked ? <DisconnectGitHub compact busy={disconnect.busy} refusal={disconnect.refusal} onDisconnect={() => void disconnect.revoke(grant.id).then(ok => { if (ok) feed.noteCredentialRevoked(grant.id); })} /> : null}
    {disconnect.remoteRevoked === false ? <Notice tone="warn" sentence="This cluster stopped using the connection, but GitHub did not confirm revocation." detail="You can remove the authorization in your GitHub settings. Other GitHub accounts and existing deployables are kept." /> : null}
    {reconnect.refusal ? <ProblemNotice problem={reconnect.refusal} tone={toneFor(reconnect.refusal.code)} /> : null}
  </>;
}
function SourceConnectionLine({ connection, grant, mode, selected, onChoose, can, returnSection, available, onRemoved }: {
  connection: SourceConnectionRow; grant?: CredentialRow; mode: "choose" | "manage"; selected: boolean;
  onChoose?: (connection: SourceConnectionRow) => void; can: boolean; returnSection: string; available: boolean; onRemoved: () => void;
}) {
  const remove = useWrite();
  const reconnect = useGithubConnect();
  const [armed, setArmed] = useState(false);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const usable = grant?.status === "active";
  return <>
    <RecordRow icon={<GitBranch size={18} aria-hidden />} name={connection.accountLogin} secondary={`${connection.accountType === "Organization" ? "Organization" : "Personal account"} · GitHub @${grant?.login || "unavailable"}`}
      state={!usable ? "Reconnect needed" : "Connected"} tone={!usable ? "warn" : "muted"} selected={mode === "choose" ? selected : undefined}
      onOpen={mode === "choose" && usable && onChoose ? () => onChoose(connection) : undefined} disabled={!available || remove.busy}
      actions={can && !armed ? <>
        {grant ? <RefreshButton label={`Reconnect GitHub @${grant.login}`} busy={reconnect.busy} onClick={() => void reconnect.connect(returnPathFor(returnSection), grant.id)} /> : null}
        <IconButton label={`Remove source ${connection.accountLogin}`} disabled={remove.busy} onClick={() => setArmed(true)}><Link2Off size={16} aria-hidden /></IconButton>
      </> : undefined} />
    {armed ? <div className="os-settings-danger">
      <Caption>Remove {connection.accountLogin} from Sources? Existing repositories and deployables remain. The shared GitHub connection and installation stay connected.</Caption>
      <div className="os-confirm-row"><Button disabled={remove.busy} onClick={() => setArmed(false)}>Cancel</Button><Button tone="danger" busy={remove.busy} onClick={() => {
        void remove.run(query => removeSourceConnection(query, connection.id)).then(ok => { if (ok && mounted.current) onRemoved(); });
      }}>Remove</Button></div>
    </div> : null}
    {remove.refusal ? <ProblemNotice problem={remove.refusal} tone={toneFor(remove.refusal.code)} /> : null}
    {reconnect.refusal ? <ProblemNotice problem={reconnect.refusal} tone={toneFor(reconnect.refusal.code)} /> : null}
  </>;
}
function AddSource({ can, initialCredentialId, credentials, credentialFeed, returnSection, onCancel, onSaved }: {
  can: boolean; initialCredentialId?: string; credentials: readonly CredentialRow[]; credentialFeed?: CredentialFeedStatus; returnSection: string; onCancel: () => void; onSaved: () => void;
}) {
  const [credentialId, setCredentialId] = useState(initialCredentialId ?? "");
  const [installationId, setInstallationId] = useState("");
  const grant = credentials.find(row => row.id === credentialId);
  const activeId = grant?.status === "active" ? grant.id : "";
  const lookup = useSourceInstallations(activeId);
  const save = useWrite();
  const connect = useGithubConnect();
  const app = useGithubApp();
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; connect.clear(); }; }, [connect.clear]);
  const returnPath = returnPathFor(returnSection);
  const appMissing = app.status !== null && !app.status.configured;
  const selected = lookup.installations.find(row => row.id === installationId && !row.suspended);
  const ready = !credentialFeed || credentialFeed.state === "live" && !credentialFeed.error;
  const savePending = useRef(false);
  const boundary = JSON.stringify([activeId, grant?.login, grant?.status, grant?.installationIds, installationId, ready, can]);
  const currentBoundary = useRef(boundary); currentBoundary.current = boundary;
  return <div className="os-stop-body">
    <Subhead>Add source</Subhead>
    {appMissing ? <GithubAppMissing app={app} returnPath={returnPath} /> : null}
    {credentialFeed?.error ? <Notice tone="error" sentence="GitHub accounts could not be read." detail={credentialFeed.error} /> : null}
    {!ready ? <><Caption>{credentialFeed?.state === "seeding" ? "Reading GitHub accounts…" : "GitHub accounts are unavailable."}</Caption><RefreshButton label="Refresh GitHub accounts" busy={credentialFeed?.state === "seeding"} onClick={() => credentialFeed?.retry()} /></> : <>
      <RecordList as="ul" label="GitHub accounts">
        {credentials.map(row => <RecordRow key={row.id} name={`@${row.login}`} secondary="GitHub account" state={credentialIsRevoked(row) ? "Disconnected" : "Connected"}
          selected={row.id === credentialId} disabled={save.busy} onOpen={() => { setCredentialId(row.id); setInstallationId(""); save.clear(); connect.clear(); }} />)}
      </RecordList>
      {!appMissing ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPath)}>Add GitHub account</Button> : null}
    </>}
    {grant && credentialIsRevoked(grant) && !appMissing ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPath, grant.id)}>Reconnect @{grant.login}</Button> : null}
    {activeId ? <section aria-label="GitHub access">
      <Subhead meta={lookup.readAt && !lookup.busy && !lookup.refusal ? lookup.installations.length : undefined}>Accounts and organizations</Subhead>
      {lookup.busy ? <Caption>Reading access…</Caption> : !lookup.readAt && !lookup.refusal ? <Caption>Access has not been read yet.</Caption> : lookup.readAt && lookup.installations.length === 0 ? <Caption>No approved installations are available for this GitHub account.</Caption> : null}
      <RecordList as="ul" label="Accounts and organizations">{lookup.installations.map(row => <RecordRow key={row.id} name={row.login} secondary={row.accountType === "Organization" ? "Organization" : "Personal account"}
        state={row.suspended ? "Suspended" : undefined} selected={row.id === installationId} disabled={row.suspended || lookup.busy || save.busy || Boolean(lookup.refusal)} onOpen={() => setInstallationId(row.id)} />)}</RecordList>
      {lookup.pending.map(row => <Caption key={row.login}>{row.login} is awaiting an organization owner's approval.</Caption>)}
      {lookup.refusal ? <ProblemNotice problem={lookup.refusal} tone={toneFor(lookup.refusal.code)} /> : null}
      {lookup.refusal && ["credential_not_found", "credential_revoked", "reconnect_required"].includes(lookup.refusal.code) ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPath, activeId)}>Reconnect GitHub</Button> : null}
      <RefreshButton label="Refresh source access" busy={lookup.busy} onClick={() => void lookup.read()} />
      {app.status?.installUrl ? <InstallLink installUrl={app.status.installUrl} /> : null}
    </section> : null}
    {save.refusal ? <ProblemNotice problem={save.refusal} tone={toneFor(save.refusal.code)} /> : null}
    {connect.refusal ? <ProblemNotice problem={connect.refusal} tone={toneFor(connect.refusal.code)} /> : null}
    <div className="os-form-row"><Button disabled={save.busy} onClick={onCancel}>Cancel adding source</Button>
      {can && ready && activeId && selected && lookup.readAt && !lookup.busy && !lookup.refusal ? <Button tone="primary" busy={save.busy} onClick={() => {
        if (savePending.current) return;
        savePending.current = true;
        const startedFor = boundary;
        void save.run(query => createSourceConnection(query, activeId, selected.id)).then(id => {
          savePending.current = false;
          if (id && mounted.current && currentBoundary.current === startedFor) onSaved();
        });
      }}>Save source</Button> : null}
    </div>
  </div>;
}
