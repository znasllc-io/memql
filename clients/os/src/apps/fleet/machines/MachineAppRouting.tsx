import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useSession } from "../../../chrome/access";
import { useOsIfPresent } from "../../../chrome/state";
import { useOsConnection } from "../../../live/connection";
import { Button, Select } from "../../../kit";
import { DetailsSection, SettingRow } from "../../../kit/DetailsSection";
import { flatten } from "../../../kit/rows";
import { PageHelp } from "../../../kit/pageHelp";
import { accessAdmits } from "../../../system/registry";
import { WeightSlider } from "../../../kit/WeightSlider";
import { ownsMachine } from "./sharing";
import type { MachineRow } from "../rows";
import "./appRouting.css";

type Mode = "auto" | "roundRobin" | "claude-code" | "codex";
type Settings = { mode: Mode; usageWeight: number; speedWeight: number; reliabilityWeight: number; revision: string };
const defaults: Settings = { mode: "auto", usageWeight: 50, speedWeight: 50, reliabilityWeight: 50, revision: "" };
const modes: { id: Mode; label: string; hint: string }[] = [
  { id: "auto", label: "Auto", hint: "Balances observed usage, speed and reliability" },
  { id: "roundRobin", label: "Round robin", hint: "Alternates between available apps for new calls" },
  { id: "claude-code", label: "Claude Code only", hint: "Uses Claude Code exclusively" },
  { id: "codex", label: "Codex only", hint: "Uses Codex exclusively" },
];
export function appRoutingFrom(raw?: Record<string, unknown>): Settings {
  return {
    mode: modes.some(m => m.id === raw?.["mode"]) ? raw!["mode"] as Mode : "auto",
    usageWeight: weight(raw?.["usageWeight"]), speedWeight: weight(raw?.["speedWeight"]), reliabilityWeight: weight(raw?.["reliabilityWeight"]),
    revision: typeof raw?.["revision"] === "string" ? raw["revision"] : "",
  };
}
function weight(value: unknown) { return typeof value === "number" && value >= 0 && value <= 100 ? value : 50; }

export function MachineAppRouting({ machine }: { machine: MachineRow }) {
  const connection = useOsConnection();
  const { access } = useSession();
  const os = useOsIfPresent();
  const [base, setBase] = useState(() => appRoutingFrom(machine.appRouting));
  const [draft, setDraft] = useState(base);
  const lastRead = useRef(base.revision);
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");
  const [announcement, setAnnouncement] = useState("");
  const dirty = JSON.stringify(draft) !== JSON.stringify(base);
  const editable = ownsMachine(machine, access?.userId ?? "") && !machine.revokedAt && !!connection;
  useEffect(() => {
    const next = appRoutingFrom(machine.appRouting);
    if (next.revision === lastRead.current) return;
    lastRead.current = next.revision;
    setBase(next);
    if (!dirty && !saving) setDraft(next);
  }, [machine.appRouting, dirty, saving]);
  async function save() {
    if (!editable || saving || !connection) return;
    setSaving(true); setNotice("");
    try {
      const result = await connection.query.fleetSetAppRouting({ registrationId: machine.id, ...draft });
      const raw = result.rows()[0];
      const row = raw ? flatten(raw) : undefined;
      if (!row || typeof row["revision"] !== "string") throw new Error("The cluster did not confirm the app settings.");
      const saved = { ...draft, revision: row["revision"] };
      setBase(saved); setDraft(saved); setAnnouncement("App routing saved.");
    } catch (error) {
      // The shell captures console errors with the focused app context.
      console.error("App routing settings could not be saved", machine.id, error);
      setNotice("App settings could not be saved. Your changes are kept.");
    } finally { setSaving(false); }
  }
  return <DetailsSection title="App routing">
    <PageHelp title="App routing">
      <p>This machine chooses an app when a new call or step starts. An active session stays with its app. Each new call receives the relevant conversation, instructions and artifacts.</p>
      <p>Auto balances reported token usage, response time and successful calls from the last two hours. Keep at least one weight above zero. Usage is not a remaining-credit estimate.</p>
      <p>Auto and Round robin use only an app this machine has found ready. An app out of usage stays out until the machine's own check finds it working again, and nobody's request is used to find out. An interrupted session that may have performed work is not automatically replayed. An Only choice never falls through to the other app.</p>
      <p>After adding credits or signing in, there is nothing to press. The machine keeps checking, within five minutes for Codex and fifteen for Claude Code, and the app comes back when a check finds it working.</p>
    </PageHelp>
    <fieldset className="fleet-app-routing-fields" disabled={!editable || saving}>
      <SettingRow label="Strategy"><Select id={`app-strategy-${machine.id}`} label="App strategy" value={draft.mode} onChange={mode => setDraft({ ...draft, mode: mode as Mode })}>
        {modes.map(m => <option key={m.id} value={m.id} data-hint={m.hint}>{m.label}</option>)}
      </Select></SettingRow>
      <SettingRow label="Usage balance"><WeightSlider label="App usage balance" hint="Favour the app with fewer reported tokens used" value={draft.usageWeight} recommended={50} readOnly={!editable || saving || draft.mode !== "auto"} onChange={usageWeight => setDraft({ ...draft, usageWeight })} /></SettingRow>
      <SettingRow label="Speed"><WeightSlider label="App speed" hint="Favour faster completed calls" value={draft.speedWeight} recommended={50} readOnly={!editable || saving || draft.mode !== "auto"} onChange={speedWeight => setDraft({ ...draft, speedWeight })} /></SettingRow>
      <SettingRow label="Reliability"><WeightSlider label="App reliability" hint="Favour successful calls" value={draft.reliabilityWeight} recommended={50} readOnly={!editable || saving || draft.mode !== "auto"} onChange={reliabilityWeight => setDraft({ ...draft, reliabilityWeight })} /></SettingRow>
      <div className="fleet-section-actions fleet-app-routing-actions">
        <Button disabled={!dirty || saving} onClick={() => { setDraft(base); setNotice(""); }}>Discard</Button>
        <Button disabled={!dirty || saving || draft.usageWeight + draft.speedWeight + draft.reliabilityWeight === 0} busy={saving} onClick={() => void save()}>Save app routing</Button>
        <Button disabled={saving} onClick={() => setDraft({ ...defaults, revision: base.revision })}>Reset to Auto</Button>
      </div>
    </fieldset>
    <p role="status" className="os-sr-only">{announcement}</p>
    {notice ? createPortal(<div role="status" className="os-app-routing-toast"><span>{notice}</span>
      {os && accessAdmits("app:logs") ? <Button onClick={() => { os.actions.openApp("logs", "search"); setNotice(""); }}>Open logs</Button> : null}
      <Button onClick={() => setNotice("")}>Dismiss</Button>
    </div>, document.body) : null}
  </DetailsSection>;
}
