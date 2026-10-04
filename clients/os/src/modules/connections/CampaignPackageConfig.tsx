import { useCallback, useEffect, useRef, useState } from "react";
import { useOsConnection } from "../../live/connection";
import { Button, Caption, Fact, Facts, Field, Notice, Panel, Select, Subhead } from "../../kit";
import { flatten } from "../../kit/rows";
import { manifestYaml } from "../../apps/deployables/packages/manifest";
import type { PackageManifest } from "../../apps/deployables/packages/rows";

type Reply = { status: string; manifest?: PackageManifest | null; yaml?: string };

/** One package file for app and email choices. Credentials never enter it. */
export function CampaignPackageConfig({ accountId, organizationName, onImported }: {
  accountId: string; organizationName: string; onImported: () => void;
}) {
  const connection = useOsConnection();
  const file = useRef<HTMLInputElement>(null);
  const pending = useRef<AbortController | null>(null);
  const [manifest, setManifest] = useState<PackageManifest | null>(null);
  const [source, setSource] = useState("");
  const [selection, setSelection] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const run = useCallback(async (action: string, text = "", selected = "", confirmed = false): Promise<Reply | null> => {
    if (!connection || pending.current) return null;
    const request = new AbortController();
    pending.current = request; setBusy(true); setError(""); setMessage("");
    try {
      const response = await connection.query.packageCampaigns({action, manifest: text, accountId, organization: selected, confirmed}, {signal: request.signal});
      if (request.signal.aborted) return null;
      const row = response.rows()[0];
      if (!row) throw Error("The cluster did not confirm the package operation.");
      return flatten(row) as unknown as Reply;
    } catch (reason) {
      if (!request.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason));
      return null;
    } finally {
      if (pending.current === request) { pending.current = null; if (!request.signal.aborted) setBusy(false); }
    }
  }, [connection, accountId]);
  const accept = useCallback((value: PackageManifest, text: string) => {
    setManifest(value); setSource(text);
    setSelection(value.campaigns?.domains.length === 1 ? value.campaigns.domains[0]?.organization || "" : "");
  }, []);
  useEffect(() => {
    void run("installed").then(reply => { if (reply?.manifest) accept(reply.manifest, manifestYaml(reply.manifest)); });
    return () => { pending.current?.abort(); pending.current = null; };
  }, [run, accept]);
  const domains = manifest?.campaigns?.domains || [];
  const chosen = domains.find(domain => domain.organization === selection);
  async function load(selected: File | undefined) {
    if (!selected) return;
    if (selected.size > 2 * 1024 * 1024) { setError("Choose a package file smaller than 2 MiB."); return; }
    try {
      const text = await selected.text();
      const reply = await run("inspect", text);
      if (reply?.manifest) accept(reply.manifest, text);
    } catch { setError("The package file could not be read."); }
  }
  async function exportConfiguration() {
    const reply = await run("export", source);
    if (!reply?.yaml || !reply.manifest) return;
    accept(reply.manifest, reply.yaml);
    const url = URL.createObjectURL(new Blob([reply.yaml], {type:"application/yaml"}));
    const link = document.createElement("a"); link.href = url; link.download = "memql-package.yaml"; link.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    setMessage("Configuration exported.");
  }
  return <Panel label="Package configuration">
    <Subhead>Package configuration</Subhead>
    <Caption>{manifest ? `${manifest.name} · memql-package.yaml` : "Load your package to keep its Deployables and add email configuration to the same file."}</Caption>
    <input ref={file} type="file" accept=".yaml,.yml,application/yaml,text/yaml" aria-label="Package file" hidden onChange={event => { void load(event.currentTarget.files?.[0]); event.currentTarget.value = ""; }}/>
    <div className="os-panel-actions">
      <Button disabled={busy} onClick={() => file.current?.click()}>Load package</Button>
      <Button disabled={busy} onClick={() => { void exportConfiguration(); }}>Export configuration</Button>
    </div>
    {domains.length ? <details><summary>Import email configuration</summary>
      <div className="os-app-stack">
        <Field label="Package organization"><Select id="campaign-package-organization" label="Package organization" value={selection} onChange={value => { if (!busy) setSelection(value); }}><option value="">Choose an organization</option>{domains.map(domain => <option key={domain.organization} value={domain.organization}>{domain.organization}</option>)}</Select></Field>
        {chosen ? <><Facts><Fact label="Import for" value={organizationName}/><Fact label="Domain" value={chosen.domain}/><Fact label="Sender" value={chosen.sender ? `${chosen.sender.username}@${chosen.domain}` : "Choose during setup"}/><Fact label="Resource group" value={manifest?.campaigns?.azure.resourceGroup || ""}/></Facts>
          <Caption>Import prepares setup. Microsoft sign-in and domain verification are still required. DNS records are references until checked with Azure.</Caption>
          <Button disabled={busy} onClick={() => { void run("import", source, selection, true).then(reply => { if (reply?.status === "imported") { setMessage("Configuration imported. Open the sending domain to continue setup."); onImported(); } }); }}>Import for {organizationName}</Button>
        </> : null}
      </div>
    </details> : null}
    {message ? <Notice tone="info" sentence={message}/> : null}
    {error ? <Notice tone="error" sentence="Package configuration could not be completed." detail={error}/> : null}
  </Panel>;
}
