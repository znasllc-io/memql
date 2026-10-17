import { useState } from "react";
import { useSession } from "../../../chrome/access";
import { Button, Caption, Notice, Switch } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { isRevoked, ownerFeaturesFrom, type MachineOwnerFeatures, type MachineRow } from "../rows";
import { ownsMachine } from "./sharing";
import type { MachineWrites } from "./useMachineWrites";

export function OwnerFeaturesGroup({ machine, writes }: { machine: MachineRow; writes: MachineWrites }) {
  const { access } = useSession();
  const current = machine.ownerFeatures ?? ownerFeaturesFrom(undefined);
  const [failed, setFailed] = useState<MachineOwnerFeatures | null>(null);
  const [pending, setPending] = useState(false);
  const owner = ownsMachine(machine, access?.userId ?? "");
  const disabled = !owner || isRevoked(machine) || pending || writes.busyId === machine.id || !writes.setOwnerFeatures;
  async function save(choice: MachineOwnerFeatures) {
    if (disabled) return;
    setPending(true); setFailed(null);
    try { if (!await writes.setOwnerFeatures?.(machine.id, choice)) setFailed(choice); }
    finally { setPending(false); }
  }
  const controls: { key: "headless" | "apps" | "computerUse"; label: string }[] = [
    { key: "headless", label: "Commands and files" },
    { key: "apps", label: "CLIs and apps" },
    { key: "computerUse", label: "Computer use" },
  ];
  return <div aria-busy={pending || undefined}>
    <PageHelp title="Owner-only features">
      <p>These features are available only to the person who enrolled this machine. Model and workbench sharing never grant access to them.</p>
      <p>Disabling a feature blocks new requests and asks Cockpit to cancel its active work. Queued work checks the setting before starting. Re-enabling allows new requests and never restarts canceled work.</p>
      <p>Enabled means permitted. Cockpit must also be connected and ready: commands require headless support, CLIs require local sign-in and consent, and computer use requires the operating system’s permissions. These controls do not change macOS permissions.</p>
    </PageHelp>
    <Caption>Turning a feature off cancels its active work and blocks new requests.</Caption>
    {controls.map(({ key, label }) => <div className="fleet-machine-setting" key={key}>
      <span className="fleet-machine-setting-name">{label}</span>
      <Switch checked={current[key]} disabled={disabled} onChange={enabled => { void save({ ...current, [key]: enabled }); }}><span className="os-sr-only">{label}</span></Switch>
    </div>)}
    {!owner ? <Caption>Only the person who enrolled this machine can change these features.</Caption> : null}
    {failed ? <Notice tone="error" sentence="The feature change was not saved." detail={writes.actionErrorScope === "features" ? writes.actionError : undefined}>
      {failed.updatedAt === current.updatedAt ? <Button disabled={disabled} onClick={() => { void save(failed); }}>Try again</Button> : <Caption>Settings changed elsewhere. Review the current choices and try again.</Caption>}
    </Notice> : null}
  </div>;
}
