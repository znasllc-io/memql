import { useEmailCatalogs } from "../catalogs";
import { useEmailBrowse, useEmailRecord, useEmailReferences } from "../useBrowse";
import { LoadMore } from "../../../kit/LoadMore";
import { useEffect, useMemo, useState } from "react";
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

export function AudiencesSection({ feeds, recordFeeds, writes, uploads, includeArchived, openRequest, onAudience, onOrganization }: { feeds: EmailFeeds; recordFeeds: EmailRecordFeeds; writes: CampaignWrites; uploads: UploadProvider; includeArchived: boolean; openRequest?: OpenRequest; onAudience?: (id: string) => void; onOrganization?: (id: string) => void }) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  const [search, setSearch] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const sites = useSites(openId !== "");
  useEmailCatalogs(["journeys", "templates", "senders"], openId !== "");
  useOpenRequest(openRequest, target => { setAdding("add" in target); setOpenId("id" in target ? target.id : ""); });
  const browse = useEmailBrowse("audiences", { search, includeArchived, confirmation: confirmation || undefined });
  const detail = useEmailRecord("audiences", openId);
  const audiences = browse.snapshot.rows.map(audienceFromRow);
  const shownIds = [...audiences.map(a => a.id), openId].filter(Boolean).join("\n");
  useEffect(() => {
    for (const audience of [...audiences, ...detail.snapshot.rows.map(audienceFromRow)]) {
      onAudience?.(audience.id);
      onOrganization?.(audience.accountId);
    }
  }, [shownIds, detail.snapshot.rows, onAudience, onOrganization]);
  const forms = recordFeeds.signupForms.snapshot.rows.map(flatten);
  const siteReferences = useEmailReferences("sites", forms.filter(form => audiences.some(a => a.id === form.audienceId)).map(form => typeof form.siteId === "string" ? form.siteId : ""));
  const counts = useSubscriberCounts(audiences.map(a => a.id));
  const key = `${includeArchived}:${confirmation}:${search}`;
  const source = useLiveView<Row, AudienceRow>(browse.source, key, rows => rows.map(audienceFromRow));
  const open = useMemo(() => recordNamed(detail.snapshot.rows, audienceFromRow, openId) ?? recordNamed(browse.snapshot.rows, audienceFromRow, openId), [detail.snapshot.rows, browse.snapshot.rows, openId]);
  if (adding) return <AddAudienceWizard writes={writes} onBack={() => setAdding(false)} onDone={id => { setAdding(false); setOpenId(id); }} />;
  if (open) return <AudiencePage key={open.id} audience={open} feeds={{ ...feeds, audiences: detail }} recordFeeds={recordFeeds} writes={writes} uploads={uploads} sites={sites} onBack={() => setOpenId("")} />;
  const siteRows = siteReferences.snapshot.rows.map(flatten);
  const failure = browse.snapshot.error;
  return <div className="os-app-stack"><AutoRefresh onRefresh={counts.reload} />
    <Head title="Audiences" meta={listCount(source?.snapshot)}>{audiences.length || search || confirmation ? <Refine iconOnly label="Refine audiences" search={search} onSearch={setSearch} chips={confirmation ? [{ id: "confirmation", label: confirmation === "required" ? "Asks to confirm" : "Doesn't ask", onRemove: () => setConfirmation("") }] : []}><Select id="email-audience-confirmation" label="Confirmation" value={confirmation} onChange={setConfirmation}><option value="">Any confirmation</option><option value="required">Asks to confirm</option><option value="none">Doesn't ask</option></Select></Refine> : null}<AddButton label="Add an audience" onClick={() => setAdding(true)} /></Head>
    {openId && !open && listKnowledge(detail) === "known" ? <Notice tone="info" sentence="That audience isn't available." /> : null}
    {[...counts.refusals.values()].map((reason, i) => <Notice key={i} tone="warn" sentence={refusalSentence(reason)} />)}
    <LiveList<AudienceRow> key={key} source={source} rowId={a => a.id} fingerprint={audienceFingerprint} label="Audiences" errorContent={<Notice tone="warn" sentence={refusalSentence(failure) || "This cluster did not return your audiences."}><Button onClick={browse.reseed}>Try again</Button></Notice>} emptyContent={<EmptyState icon={Users} title={search || confirmation ? "No matching audiences" : "No audiences yet"}>{search || confirmation ? "Try another name or confirmation choice." : audiences.some(audienceIsArchived) ? "Archived audiences are hidden. Turn on Include archived in Settings to see them." : "Add an audience, then import a CSV or add one address."}</EmptyState>} renderRow={(audience, tick) => {
      const count = counts.counts.get(audience.id);
      return <RecordRow icon={<Users size={16} aria-hidden />} name={audienceName(audience)} secondary={audience.description} state={audienceIsArchived(audience) ? "Archived" : "Active"} dim={audienceIsArchived(audience)} stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null} onOpen={() => setOpenId(audience.id)}>
        {count?.kind === "measured" ? <span>{WORDS.subscribers(count)}</span> : count?.kind === "absent" && count.reason !== "unread" ? <span>—</span> : <InlineSkeleton label="Loading subscribers" />}
        {audience.confirmation === "required" ? <span>Asks to confirm</span> : null}
        {forms.filter(f => f.audienceId === audience.id && f.enabled).map(form => { const site = siteRows.find(s => s.id === form.siteId); return site ? <span key={String(form.id)}>Form on {String(site.title || site.hostname)}</span> : null; })}
      </RecordRow>;
    }} />
    <LoadMore feed={browse} />
  </div>;
}
