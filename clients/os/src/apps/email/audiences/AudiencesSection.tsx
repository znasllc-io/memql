import { useMemo, useState } from "react";
import { Users } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, EmptyState, Head, InlineSkeleton, LiveList, Notice, RecordRow, Refine, Select, listCount } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { useLiveView } from "../../../live/liveView";
import type { UploadProvider } from "../../../items/upload";
import { useSites } from "../../deployables/useSites";
import { flatten } from "../../../kit/rows";
import type { CampaignWrites } from "../actions";
import { audienceFingerprint, audienceFromRow, audienceIsArchived, audienceName, type AudienceRow } from "../rows";
import { listKnowledge, useSubscriberCounts, type EmailFeeds, type EmailRecordFeeds } from "../useEmail";
import { recordNamed, useOpenRequest, type OpenRequest } from "../openRequest";
import { WORDS } from "../words";
import { AddAudienceWizard } from "./AddAudienceWizard";
import { AudiencePage } from "./AudiencePage";

export function AudiencesSection({ feeds, recordFeeds, writes, uploads, includeArchived, openRequest }: { feeds: EmailFeeds; recordFeeds: EmailRecordFeeds; writes: CampaignWrites; uploads: UploadProvider; includeArchived: boolean; openRequest?: OpenRequest }) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  const [search, setSearch] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const sites = useSites();
  useOpenRequest(openRequest, target => { setAdding("add" in target); setOpenId("id" in target ? target.id : ""); });
  const audiences = feeds.audiences.snapshot.rows.map(audienceFromRow);
  const counts = useSubscriberCounts(audiences.map(a => a.id));
  const key = `${includeArchived}:${confirmation}:${search}`;
  const source = useLiveView<Row, AudienceRow>(feeds.audiences.source, key, rows => rows.map(audienceFromRow).filter(a => a.id && (includeArchived || !audienceIsArchived(a)) && (!confirmation || a.confirmation === confirmation) && `${a.name} ${a.description}`.toLowerCase().includes(search.trim().toLowerCase())));
  const open = useMemo(() => recordNamed(feeds.audiences.snapshot.rows, audienceFromRow, openId), [feeds.audiences.snapshot.rows, openId]);
  if (adding) return <AddAudienceWizard writes={writes} onBack={() => setAdding(false)} onDone={id => { setAdding(false); setOpenId(id); }} />;
  if (open) return <AudiencePage key={open.id} audience={open} feeds={feeds} recordFeeds={recordFeeds} writes={writes} uploads={uploads} sites={sites} onBack={() => setOpenId("")} />;
  const forms = recordFeeds.signupForms.snapshot.rows.map(flatten);
  const siteRows = sites.snapshot.rows.map(flatten);
  const failure = feeds.audiences.snapshot.error;
  return <div className="os-app-stack"><AutoRefresh onRefresh={counts.reload} />
    <Head title="Audiences" meta={listCount(source?.snapshot)}>{audiences.length ? <Refine iconOnly label="Refine audiences" search={search} onSearch={setSearch} chips={confirmation ? [{ id: "confirmation", label: confirmation === "required" ? "Asks to confirm" : "Doesn't ask", onRemove: () => setConfirmation("") }] : []}><Select id="email-audience-confirmation" label="Confirmation" value={confirmation} onChange={setConfirmation}><option value="">Any confirmation</option><option value="required">Asks to confirm</option><option value="none">Doesn't ask</option></Select></Refine> : null}<AddButton label="Add an audience" onClick={() => setAdding(true)} /></Head>
    {openId && !open && listKnowledge(feeds.audiences) === "known" && !feeds.audiences.snapshot.truncated ? <Notice tone="info" sentence="That audience isn't available." /> : null}
    {[...counts.refusals.values()].map((reason, i) => <Notice key={i} tone="warn" sentence={refusalSentence(reason)} />)}
    <LiveList<AudienceRow> key={key} source={source} rowId={a => a.id} fingerprint={audienceFingerprint} label="Audiences" errorContent={<Notice tone="warn" sentence={refusalSentence(failure) || "This cluster did not return your audiences."}><Button onClick={feeds.audiences.reseed}>Try again</Button></Notice>} emptyContent={<EmptyState icon={Users} title={search || confirmation ? "No matching audiences" : "No audiences yet"}>{search || confirmation ? "Try another name or confirmation choice." : audiences.some(audienceIsArchived) ? "Archived audiences are hidden. Turn on Include archived in Settings to see them." : "Add an audience, then import a CSV or add one address."}</EmptyState>} renderRow={(audience, tick) => {
      const count = counts.counts.get(audience.id);
      return <RecordRow icon={<Users size={16} aria-hidden />} name={audienceName(audience)} secondary={audience.description} state={audienceIsArchived(audience) ? "Archived" : "Active"} dim={audienceIsArchived(audience)} stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null} onOpen={() => setOpenId(audience.id)}>
        {count?.kind === "measured" ? <span>{WORDS.subscribers(count)}</span> : count?.kind === "absent" && count.reason !== "unread" ? <span>—</span> : <InlineSkeleton label="Loading subscribers" />}
        {audience.confirmation === "required" ? <span>Asks to confirm</span> : null}
        {forms.filter(f => f.audienceId === audience.id && f.enabled).map(form => { const site = siteRows.find(s => s.id === form.siteId); return site ? <span key={String(form.id)}>Form on {String(site.title || site.hostname)}</span> : null; })}
      </RecordRow>;
    }} />
  </div>;
}
