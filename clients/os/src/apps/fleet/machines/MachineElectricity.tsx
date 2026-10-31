import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Caption, Input, Notice, Select, Fact, Facts } from "../../../kit";
import { DetailsSection, SettingRow } from "../../../kit/DetailsSection";
import { PageHelp } from "../../../kit/pageHelp";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useOpenAskWork } from "../../../ask/AskProvider";
import { flatten } from "../../../kit/rows";
import type { MachineRow } from "../rows";

type Tariff = { provider: string; name: string; currency: string; checkedAt: string; refreshAfter: string; effectiveFrom: string; sourceUrls: string[]; rateBands: { name: string; perKWh: number; appliesWhen: string }[]; assumptions: string[] };
type Research = { status?: string; candidates?: { name: string; sourceUrl: string; evidence: string }[]; tariffs?: Tariff[]; notes?: string };
type Settings = { address: string; city: string; region: string; postalCode: string; country: string; timezone: string; provider: string; watts: number | null; energySource: string; verified: boolean; lookupAllowed: boolean; revision: string; locationRevision: string; research: Research; tariff: Partial<Tariff> };
const empty: Settings = { address: "", city: "", region: "", postalCode: "", country: "", timezone: "", provider: "", watts: null, energySource: "", verified: false, lookupAllowed: false, revision: "", locationRevision: "", research: {}, tariff: {} };
export function electricitySettingsFrom(row: Record<string, unknown>): Settings { return { ...empty, ...row } as Settings; }

/** Contains no caller-visible machine address in a goal, URL or diagnostic. */
export function electricityResearchRequest(s: Settings, registrationId: string) {
  return {
    statement: "Research electricity utilities and applicable residential/commercial tariffs for this machine's authorized locality. Use any available authorized inference door and public official service-area and tariff sources. Do not fetch, request or disclose a precise street address. Do not infer a utility solely from the city. Retain candidates with citations; if multiple utilities or tariffs may apply, mark ambiguous for owner confirmation. Use the supplied provider when given, checking its service area. Preserve effective and checked dates, currency, timezone, all time-of-use/tier bands, taxes/fees and missing power measurements. Submit bounded findings through recordElectricityResearch using the supplied registrationId and locationRevision; include unavailable lookup honestly. No machine settings, routing or account changes are authorized.",
    input: { registrationId, locationRevision: s.locationRevision, locality: { city: s.city, region: s.region, postalCode: s.postalCode, country: s.country, timezone: s.timezone }, provider: s.provider },
    requestedVia: "nexus" as const,
  };
}

export function MachineElectricity({ machine }: { machine: MachineRow }) {
  const connection = useOsConnection(); const { access } = useSession(); const openWork = useOpenAskWork();
  const [base, setBase] = useState<Settings>(empty); const [draft, setDraft] = useState<Settings>(empty);
  const [busy, setBusy] = useState(false); const [reading, setReading] = useState(false); const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState(""); const [run, setRun] = useState(""); const [selected, setSelected] = useState("");
  const writing = useRef(false); const readGeneration = useRef(0); const dirty = JSON.stringify(draft) !== JSON.stringify(base); const dirtyRef = useRef(dirty); dirtyRef.current = dirty;
  const editable = machine.ownerUserId === access?.userId && !machine.revokedAt;
  const read = useCallback(async () => {
    if (!connection || !editable || writing.current) return;
    const generation = ++readGeneration.current; setReading(true);
    try {
      const result = await connection.query.fleetEnergySettings({ registrationId: machine.id, action: "read" });
      if (generation !== readGeneration.current || writing.current) return;
      const raw = result.rows()[0]; if (!raw) throw new Error("No settings returned.");
      const next = electricitySettingsFrom(flatten(raw)); setLoaded(true);
      // Do not adopt a newer revision underneath an older unsaved draft. A
      // conflict must keep refusing until the owner discards and reloads.
      if (!dirtyRef.current) { setBase(next); setDraft(next); }
      setError(previous => previous.startsWith("Electricity settings could not be read.") ? "" : previous);
    } catch { if (generation === readGeneration.current) setError("Electricity settings could not be read. Try again when connected."); }
    finally { if (generation === readGeneration.current) setReading(false); }
  }, [connection, editable, machine.id]);
  useEffect(() => { void read(); return () => { readGeneration.current++; }; }, [read]);
  function change(key: keyof Settings, value: string | number | null) {
    setDraft(old => ({ ...old, [key]: value, ...(["address", "city", "region", "postalCode", "country", "timezone"].includes(key) ? { verified: false } : {}) }));
  }
  async function research(saved: Settings) {
    if (!connection || !openWork) throw new Error("Ask is unavailable. The settings were saved; retry the utility lookup when Ask reconnects.");
    const receipt = await connection.query.createGoal(electricityResearchRequest(saved, machine.id));
    const row = receipt.rows()[0]; const created = row ? flatten(row)["runId"] : undefined; if (typeof created !== "string" || !created) throw new Error("Utility research was not confirmed. Settings remain saved.");
    setRun(created); openWork(created);
  }
  async function save(lookup: boolean) {
    if (!connection || writing.current || !editable) return;
    writing.current = true; readGeneration.current++; setBusy(true); setError("");
    try {
      const settings = { address: draft.address, city: draft.city, region: draft.region, postalCode: draft.postalCode, country: draft.country, timezone: draft.timezone, provider: draft.provider, watts: draft.watts, energySource: draft.energySource, verified: lookup || draft.verified, lookupAllowed: lookup || draft.lookupAllowed };
      const result = await connection.query.fleetEnergySettings({ registrationId: machine.id, action: "save", revision: base.revision, secretSettings: settings });
      const raw = result.rows()[0]; if (!raw) throw new Error("Save was not confirmed."); const receipt = flatten(raw);
      const saved = { ...draft, ...settings, revision: String(receipt["revision"]), locationRevision: String(receipt["locationRevision"]) };
      setBase(saved); setDraft(saved); dirtyRef.current = false;
      if (lookup) await research(saved);
    } catch (err) { setError(err instanceof Error ? err.message : "Save failed. Your draft is kept."); }
    finally { writing.current = false; setBusy(false); void read(); }
  }
  async function confirm() {
    const tariff = base.research.tariffs?.find(t => JSON.stringify(t) === selected); if (!connection || !tariff || writing.current || dirty) return;
    writing.current = true; setBusy(true); setError("");
    try { await connection.query.fleetEnergySettings({ registrationId: machine.id, action: "confirm", revision: base.revision, provider: tariff.provider, tariffName: tariff.name }); }
    catch { setError("The tariff could not be confirmed. Reload the findings and retry."); }
    finally { writing.current = false; setBusy(false); void read(); }
  }
  if (!editable) return null;
  const field = (key: keyof Settings, label: string) => <SettingRow label={label}><Input id={`energy-${key}-${machine.id}`} label={label} value={String(draft[key] ?? "")} onChange={v => change(key, v)} disabled={busy || !loaded} /></SettingRow>;
  const current = base.tariff; const stale = !!current.refreshAfter && Date.now() >= Date.parse(current.refreshAfter);
  return <DetailsSection title="Electricity">
    <AutoRefresh enabled={!dirty && !busy} busy={reading} onRefresh={read} />
    <PageHelp title="Electricity"><p>Enter the machine's physical service address. It is sealed in owner-only settings. Verifying it authorizes utility research with only city, region, postcode and country. Exact addresses never enter research goals or public evaluation evidence.</p><p>MemQL retains candidate utilities and cited tariffs for you to confirm. A location or provider change clears prior findings. If lookup fails, enter the provider and retry. Tariff choice may require your bill; never upload account numbers.</p><p>Estimated watts must describe this machine's power during inference and cite the basis, such as a power meter sample or an explicit owner estimate. Tokens cannot determine electricity. Local-call estimates multiply watts by duration and the confirmed tariff; overlapping calls can double count whole-machine power. Unknown time-of-use/tier allocation produces a range. No power estimate means unavailable cost.</p></PageHelp>
    {field("address", "Service address")}{field("city", "City")}{field("region", "State or region")}{field("postalCode", "Postal code")}{field("country", "Country code")}{field("timezone", "Timezone")}
    <Caption>{base.verified ? "Address verified by you. Only locality is used for utility lookup." : "Verify the address to authorize lookup using city, region, postcode and country."}</Caption>
    {field("provider", "Utility provider")}
    <div className="fleet-section-actions"><Button disabled={!connection || !loaded || busy || !draft.address || !draft.city || !draft.postalCode || draft.country.length !== 2 || !draft.timezone} busy={busy} onClick={() => void save(true)}>Verify address and find utility</Button>{run ? <Button onClick={() => openWork?.(run)}>View research</Button> : null}</div>
    {base.research.status ? <Facts><Fact label="Lookup" value={base.research.status === "unavailable" ? "Unavailable — enter your provider and retry" : base.research.status === "ambiguous" ? "Confirmation needed" : "Findings ready for confirmation"} /></Facts> : null}
    {base.research.candidates?.map((c, i) => <p key={`${c.name}-${i}`}><a href={safeCitation(c.sourceUrl)} target="_blank" rel="noreferrer">{c.name}</a> — {c.evidence}</p>)}
    {base.research.notes ? <Caption>{base.research.notes}</Caption> : null}
    {base.research.tariffs?.length ? <><SettingRow label="Tariff"><Select id={`energy-tariff-${machine.id}`} label="Utility tariff" value={selected} onChange={setSelected}><option value="">Choose your applicable tariff</option>{base.research.tariffs.map((t, i) => <option key={`${t.provider}-${t.name}-${i}`} value={JSON.stringify(t)}>{t.provider} — {t.name}</option>)}</Select></SettingRow><div className="fleet-section-actions"><Button disabled={!base.research.tariffs?.some(t => JSON.stringify(t) === selected) || busy || dirty} onClick={() => void confirm()}>Confirm utility and tariff</Button></div></> : null}
    {current.name ? <><Facts><Fact label="Confirmed tariff" value={`${current.provider} — ${current.name}`} /><Fact label="Price evidence" value={stale ? "Stale — research rates again" : `Checked ${current.checkedAt}`} /><Fact label="Effective from" value={current.effectiveFrom} /></Facts>{current.rateBands?.map((b, i) => <p key={i}>{b.name}: {b.perKWh} {current.currency}/kWh · {b.appliesWhen}</p>)}{current.sourceUrls?.map((url, i) => <p key={i}><a href={safeCitation(url)} target="_blank" rel="noreferrer">Tariff source {i + 1}</a></p>)}</> : <Caption>No confirmed tariff. Electricity estimates are unavailable.</Caption>}
    <SettingRow label="Estimated watts"><Input id={`energy-watts-${machine.id}`} label="Estimated whole-machine watts" value={draft.watts === null ? "" : String(draft.watts)} disabled={busy || !loaded} onChange={v => { if (v === "" || /^\d*(\.\d*)?$/.test(v)) change("watts", v === "" ? null : Number(v)); }} /></SettingRow>
    {field("energySource", "Estimate source")}
    <div className="fleet-section-actions"><Button disabled={!dirty || busy} onClick={() => { dirtyRef.current = false; setDraft(base); setError(""); void read(); }}>Discard</Button><Button disabled={!dirty || busy || !loaded || !connection || !draft.verified} onClick={() => void save(false)}>Save energy estimate</Button></div>
    {error ? <Notice tone="error" sentence={error} /> : null}
  </DetailsSection>;
}
function safeCitation(value: string) { try { const u = new URL(value); return u.protocol === "https:" ? value : undefined; } catch { return undefined; } }
