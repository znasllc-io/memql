import { useEffect, useRef, useState } from "react";
import { GitBranch } from "lucide-react";
import { Button, Caption, Fact, Facts, Notice, RecordList, RecordRow, type Stop } from "../../kit";
import { Wizard } from "../../kit/Wizard";
import { useSourceConnections } from "../../modules/connections/connections";
import { useRegisterRepositorySource } from "../deployables/packages/actions";
import { RepositoryPicker } from "../deployables/sources/RepositoryPicker";
import { useSourceRepositories } from "../deployables/sources/useGithubConnect";
import type { RepositoryRow } from "../deployables/sources/repositories";
import { useAccountOptionsFeed } from "../accounts/tie";
import { accountName } from "../accounts/rows";

export function AddRepository({ back, onAdded, onSettings }: { back: () => void; onAdded: (id: string) => void; onSettings: () => void }) {
  const connections = useSourceConnections();
  const accounts = useAccountOptionsFeed();
  const registration = useRegisterRepositorySource();
  const repositories = useSourceRepositories();
  const [connectionId, setConnectionId] = useState("");
  const [repository, setRepository] = useState<RepositoryRow | null>(null);
  const [account, setAccount] = useState<string | null>(null);
  const [step, setStep] = useState("access");
  const connection = connections.rows.find(row => row.id === connectionId);
  const read = repositories.read;
  useEffect(() => { if (connection) void read(connection.credentialId, 1, connection.id); }, [connection?.id, connection?.credentialId, read]);
  const submitting = useRef(false);
  async function save() {
    if (submitting.current || !connection || !repository || account === null) return;
    submitting.current = true;
    try {
      const id = await registration.create({ name: repository.name, sourceKind: "repo", repoUrl: repository.url, repoRef: repository.defaultBranch,
        autoDeploy: false, credentialId: connection.credentialId, sourceConnectionId: connection.id, artifactId: "", accountId: account });
      if (id) onAdded(id);
    } finally { submitting.current = false; }
  }
  const stages = ["access", "repository", "ownership", "review"];
  const index = stages.indexOf(step);
  const steps: Stop[] = [
    { id: "access", name: "GitHub account", state: connection ? "done" : "open", answer: connection?.accountLogin,
      body: <><RecordList as="ul" label="GitHub connections">{connections.rows.map(row => <RecordRow key={row.id} name={row.accountLogin} secondary={row.accountType} onOpen={() => { setConnectionId(row.id); setRepository(null); setStep("repository"); }} />)}</RecordList>
        {connections.error ? <Notice tone="warn" sentence="GitHub access could not be refreshed." detail={connections.error} /> : null}
        {!connections.rows.length ? <Caption>{connections.state === "seeding" ? "Reading GitHub connections…" : "Connect a GitHub account to choose a repository."}</Caption> : null}
        <Button tone="quiet" onClick={onSettings}>Manage GitHub connections</Button></> },
    { id: "repository", name: "Repository", state: repository ? "done" : connection ? "open" : "ahead", answer: repository?.fullName,
      body: connection ? <RepositoryPicker page={repositories.page} readAt={repositories.readAt} busy={repositories.busy} refusal={repositories.refusal}
        chosen={repository?.fullName} onChoose={row => { setRepository(row); setStep("ownership"); }}
        onLookAgain={() => void read(connection.credentialId, 1, connection.id)} onReadMore={() => void read(connection.credentialId, repositories.page.nextPage, connection.id)} /> : undefined },
    { id: "ownership", name: "MemQL organization", state: account !== null ? "done" : repository ? "open" : "ahead", answer: account === "" ? "Personal" : accounts.accounts.find(row => row.id === account)?.name,
      body: <><RecordList as="ul" label="Repository organization"><RecordRow name="Personal" onOpen={() => { setAccount(""); setStep("review"); }} />{accounts.accounts.filter(row => row.status !== "archived").map(row => <RecordRow key={row.id} name={accountName(row)} onOpen={() => { setAccount(row.id); setStep("review"); }} />)}</RecordList>{accounts.error ? <Notice tone="warn" sentence="Organizations could not be refreshed." detail={accounts.error} /> : null}</> },
    { id: "review", name: "Review", state: account !== null && repository ? "open" : "ahead", body: <><Facts><Fact label="Repository" value={repository?.fullName} /><Fact label="Branch" value={repository?.defaultBranch} /><Fact label="Access through" value={connection?.accountLogin} /></Facts><Caption>Register this source. Its workflow is connected separately; adding it starts no deployment.</Caption></> },
  ];
  return <Wizard icon={<GitBranch size={24} />} title="Add repository" label="Add repository steps" back={{ label: "Repositories", onSelect: () => { if (!submitting.current) back(); } }} steps={steps} open={step} onOpen={next => { if (!submitting.current) setStep(next); }}
    status={{ word: registration.busy ? "Adding repository" : steps[index]?.name ?? "Add repository", tone: registration.busy ? "busy" : "none" }}
    notices={registration.refusal ? <Notice tone="error" sentence="The repository was not added." detail={registration.refusal.message} /> : null}
    acts={[...(registration.busy ? [] : [{ label: "Cancel", text: true, onAct: () => { if (!submitting.current) back(); } }]), ...(index > 0 && !registration.busy ? [{ label: "Back", text: true, onAct: () => { if (!submitting.current) setStep(stages[index - 1]!); } }] : []), ...(step === "review" && account !== null && repository && connection ? [{ label: "Add repository", tone: "primary" as const, busy: registration.busy, onAct: () => void save() }] : [])]} />;
}
