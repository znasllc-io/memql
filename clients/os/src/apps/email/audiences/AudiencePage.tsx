import { emailState } from "../words";
import { EMAIL_REFRESH_MS } from "../useEmail";
import { useMemo, useRef, useState } from "react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Fact, Facts, Head, InlineSkeleton, Notice, Panel, formatMoment } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { countFigure } from "../../../kit/measure";
import { flatten } from "../../../kit/rows";
import type { UploadProvider } from "../../../items/upload";
import { useOsConnection } from "../../../live/connection";
import type { LiveCollectionHandle } from "../../../live/useLiveCollection";
import { useAccountOptions } from "../../accounts/tie";
import { useAudienceLifecycle, type CampaignWrites } from "../actions";
import { toKitActs } from "../acts";
import { organizationName } from "../organizations";
import { journeyFromRow, type AudienceRow } from "../rows";
import { useAudienceRecipients, useReading, useSubscriberCounts, type EmailFeeds, type EmailRecordFeeds } from "../useEmail";
import { audienceActs } from "./audienceActs";
import { AddPeopleWizard } from "./AddPeopleWizard";
import { Subscribers } from "./Subscribers";
import { ConnectSignupFormWizard, SignupFormPage, SignupForms } from "./SignupForms";

export function AudiencePage({ audience, feeds, recordFeeds, writes, uploads, sites, onBack }: { audience: AudienceRow; feeds: EmailFeeds; recordFeeds: EmailRecordFeeds; writes: CampaignWrites; uploads: UploadProvider; sites: LiveCollectionHandle<Row>; onBack: () => void }) {
  const [view, setView] = useState("");
  const [asking, setAsking] = useState<"" | "archive" | "confirmation">("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const working = useRef(false);
  const accounts = useAccountOptions();
  const lifecycle = useAudienceLifecycle();
  const roster = useAudienceRecipients(audience.id);
  const counts = useSubscriberCounts([audience.id]);
  const connection = useOsConnection();
  const readSize = useMemo(() => connection ? async (signal: AbortSignal) => countFigure((await connection.query.audienceRosterSize({ audienceId: audience.id }, { signal })).count()) : null, [connection, audience.id]);
  const size = useReading<ReturnType<typeof countFigure> | null>(null, readSize, [readSize]);
  const count = counts.counts.get(audience.id);
  const figure = (f: typeof count) => !f || f.kind === "absent" && f.reason === "unread" ? <InlineSkeleton label="Loading count" /> : f.kind === "measured" ? `${f.value.toLocaleString()}${f.atLeast ? " or more" : ""}` : "—";
  const binding = recordFeeds.signupForms.snapshot.rows.find(r => flatten(r).id === view);
  const shared = { audience, feeds, signupForms: recordFeeds.signupForms, sites };
  async function change(id: string) {
    if (id === "keep") { setAsking(""); return; }
    if (id === "archive" || id === "confirmation") { setAsking(id); setError(""); return; }
    if (working.current) return; working.current = true; setBusy(true); setError("");
    try {
      const done = asking === "archive" ? await writes.archiveAudience.archive(audience.id) : await lifecycle.call(audience.id, id === "restore" ? "restore" : "confirmation", audience.confirmation === "required" ? "none" : "required");
      if (done) { setAsking(""); feeds.audiences.reseed(); }
      else setError("The audience wasn't changed.");
    } finally { working.current = false; setBusy(false); }
  }
  if (view === "people") return <AddPeopleWizard audience={audience} writes={writes} uploads={uploads} onBack={() => setView("")} onDone={() => { setView(""); void roster.reload(); void counts.reload(); void size.reload(); }} />;
  if (view === "connect") return <ConnectSignupFormWizard {...shared} onBack={() => setView("")} onDone={() => setView("")} />;
  if (binding) return <SignupFormPage {...shared} binding={binding} onBack={() => setView("")} />;
  const question = asking === "archive" ? `Archive “${audience.name}”? It leaves the campaign and automation pickers; its history stays.` : asking === "confirmation" ? audience.confirmation === "required" ? "Stop asking to confirm? New signups are subscribed right away." : "Ask people to confirm? New signups wait as pending until they confirm." : "";
  return <div className="os-action-pane"><div className="os-action-body os-app-stack"><Head title={audience.name} back={{ label: "Audiences", onSelect: onBack }} />
    <AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={async () => { await Promise.all([roster.reload(), counts.reload(), size.reload()]); }} busy={roster.state === "loading" || size.state === "loading"} />
    <Panel label="Audience details"><Facts><Fact label="Organization" value={organizationName(accounts, audience.accountId)} /><Fact label="Description" value={audience.description} /><Fact label="Subscribers" value={figure(count)} /><Fact label="On the list" value={figure(size.value ?? undefined)} /><Fact label="Confirmation" value={audience.confirmation === "required" ? "Asks people to confirm" : "Doesn't ask"} /><Fact label="Created" value={formatMoment(audience.createdAt)} /></Facts></Panel>
    {size.error || counts.refusals.get(audience.id) ? <Notice tone="warn" sentence={refusalSentence(size.error || counts.refusals.get(audience.id) || "")} /> : null}
    <Subscribers roster={roster} writes={writes} onAdd={() => setView("people")} archived={audience.status === "archived"} />
    {feeds.journeys.snapshot.rows.map(journeyFromRow).filter(j => j.status === "active" && j.trigger.kind === "joined" && j.trigger.audienceId === audience.id).map(j => <p className="os-caption" key={j.id}>People you add join “{j.name}”.</p>)}
    <SignupForms audience={audience} signupForms={recordFeeds.signupForms} sites={sites} onAdd={() => setView("connect")} onOpen={setView} />
    {error ? <Notice tone="warn" sentence={refusalSentence(lifecycle.error || writes.archiveAudience.error) || error} /> : null}
  </div><ActionBar state={busy ? "Saving" : emailState("audience", audience.status)} tone={busy ? "busy" : "none"} acts={busy ? [] : toKitActs(audienceActs(audience, asking), id => { void change(id); })}>{question ? <span className="os-actbar-confirm">{question}</span> : null}</ActionBar></div>;
}
