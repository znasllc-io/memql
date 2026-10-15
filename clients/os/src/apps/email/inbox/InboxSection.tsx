import { useCallback, useEffect, useRef, useState } from "react";
import { Mail } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, EmptyState, Head, Notice, RecordList, RecordRow, RecordListSkeleton, Refine, Select, formatMoment } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useAccountOptions } from "../../accounts/tie";
import { organizationName } from "../organizations";
import { MessagePage } from "./MessagePage";

export interface CapturedEmail {
  id: string; createdAt: string; accountId?: string;
  from: string; fromName: string; To: string; Subject: string;
  TextBody: string; HTMLBody: string; Headers?: Record<string, string>;
}

/** Captured messages are encrypted and not broadcast. Visible reads use the
 * shared refresh cadence and cancel stale answers when the session changes. */
export function InboxSection() {
  const connection = useOsConnection();
  const accounts = useAccountOptions();
  const [messages, setMessages] = useState<CapturedEmail[]>([]);
  const [mode, setMode] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [settled, setSettled] = useState(false);
  const [selected, setSelected] = useState("");
  const [search, setSearch] = useState("");
  const [organization, setOrganization] = useState("");
  const generation = useRef(0);
  const refresh = useCallback(async () => {
    const revision = ++generation.current;
    if (!connection) { setMessages([]); setMode(""); setLoading(false); setError(""); setSettled(false); return; }
    setLoading(true); setError("");
    try {
      const result = await connection.query.emailInbox({});
      if (revision !== generation.current) return;
      const first = result.rows()[0];
      if (!first) throw new Error("The inbox returned no result.");
      const data = flatten(first);
      setMode(typeof data.mode === "string" ? data.mode : "");
      setMessages(Array.isArray(data.messages) ? data.messages as CapturedEmail[] : []);
      setSettled(true);
    } catch (failure) {
      if (revision === generation.current) setError(refusalSentence(failure, "emailInbox"));
    } finally { if (revision === generation.current) setLoading(false); }
  }, [connection]);
  useEffect(() => { void refresh(); return () => { generation.current++; }; }, [refresh]);
  const message = messages.find(item => item.id === selected);
  const organizations = [...new Set(messages.map(item => item.accountId).filter((id): id is string => !!id))];
  const needle = search.trim().toLowerCase();
  const filtered = messages.filter(item => (!organization || item.accountId === organization) && `${item.Subject} ${item.To} ${item.from} ${item.fromName}`.toLowerCase().includes(needle));
  return <div className="os-app-stack">
    <AutoRefresh busy={loading || !connection} onRefresh={() => { void refresh(); }} />
    {message ? <MessagePage message={message} onBack={() => setSelected("")} /> : <>
      <Head title="Inbox" meta={mode === "capture" ? "Past 7 days" : undefined}>
        {mode === "capture" && messages.length > 0 ? <Refine iconOnly label="Refine inbox" search={search} onSearch={setSearch} chips={organization ? [{ id: "organization", label: organizationName(accounts, organization), onRemove: () => setOrganization("") }] : []}>
          {organizations.length > 1 ? <Select id="email-inbox-organization" label="Organization" value={organization} onChange={setOrganization}><option value="">Any organization</option>{organizations.map(id => <option key={id} value={id}>{organizationName(accounts, id)}</option>)}</Select> : null}
        </Refine> : null}
      </Head>
      {!settled && loading ? <RecordListSkeleton label="Loading email" /> : null}
      {mode === "external" ? <EmptyState icon={Mail} title="Nothing is captured on this cluster">This cluster delivers email to real inboxes.</EmptyState> : null}
      {mode === "capture" ? !messages.length ? <EmptyState icon={Mail} title="No test email yet">Email this cluster sends appears here instead of being delivered.</EmptyState> : !filtered.length ? <EmptyState title="No matching email">Try another subject or address.</EmptyState> : <RecordList label="Captured messages">{filtered.map(item => <RecordRow key={item.id} icon={<Mail size={16} aria-hidden />} name={item.Subject} secondary={item.To} trailing={formatMoment(item.createdAt)} onOpen={() => setSelected(item.id)} />)}</RecordList> : null}
    </>}
    {error ? <Notice tone="warn" sentence="Could not read email." detail={error}><Button busy={loading} onClick={() => { void refresh(); }}>Try again</Button></Notice> : null}
  </div>;
}
