import { useCallback, useEffect, useRef, useState } from "react";
import { Mail } from "lucide-react";
import { Button, EmptyState, Fact, Facts, Head, RefreshButton, Input, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, formatMoment } from "../../kit";
import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { emailPreview } from "./preview";

export interface CapturedEmail {
  id: string; createdAt: string; accountId?: string;
  from: string; fromName: string; To: string; Subject: string;
  TextBody: string; HTMLBody: string; Headers?: Record<string, string>;
}

/** A bounded, explicitly refreshed test inbox. Encrypted messages are not
 * broadcast over the mesh; focus and Refresh re-read shared storage. */
export function EmailApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  if (sectionId === "logs") return <AppLogsSection app="email" subjectConcepts={["v1:email:capturedMessage"]} intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "settings") return <div className="os-settings"><Head title="Email settings" /><Panel label="Email delivery"><p>Delivery is managed in Settings, under Integrations. A capture transport keeps test messages in this installation. Connecting an incoming mail server will be available in a future update.</p></Panel></div>;
  return <EmailInbox />;
}

export function EmailInbox() {
  const connection = useOsConnection();
  const [messages, setMessages] = useState<CapturedEmail[]>([]);
  const [mode, setMode] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [readAt, setReadAt] = useState("");
  const [selected, setSelected] = useState("");
  const [search, setSearch] = useState("");
  const [plain, setPlain] = useState(false);
  const generation = useRef(0);
  const refresh = useCallback(async () => {
    const revision = ++generation.current;
    if (!connection) { setMessages([]); setMode(""); setLoading(false); setError(""); setReadAt(""); return; }
    setLoading(true); setError("");
    try {
      const result = await connection.query.emailInbox({});
      if (revision !== generation.current) return;
      const first = result.rows()[0];
      if (!first) throw new Error("The inbox returned no result.");
      const data = flatten(first);
      setMode(typeof data.mode === "string" ? data.mode : "");
      setMessages(Array.isArray(data.messages) ? data.messages as CapturedEmail[] : []);
      setReadAt(new Date().toISOString());
    } catch (failure) {
      if (revision === generation.current) {
        setError(failure instanceof Error ? failure.message : "Could not read email.");
      }
    } finally { if (revision === generation.current) setLoading(false); }
  }, [connection]);
  useEffect(() => {
    void refresh();
    const focus = () => { void refresh(); };
    window.addEventListener("focus", focus);
    return () => { generation.current++; window.removeEventListener("focus", focus); };
  }, [refresh]);
  const message = messages.find((item) => item.id === selected);
  if (message) return <div className="os-app-stack">
    <Head title={message.Subject} back={{ label: "Inbox", onSelect: () => setSelected("") }} meta="Captured for testing" />
    <Panel label="Message details"><Facts>
      <Fact label="From" value={`${message.fromName} <${message.from}>`} />
      <Fact label="To" value={message.To} />
      <Fact label="Captured" value={formatMoment(message.createdAt)} />
      {message.Headers?.["Reply-To"] ? <Fact label="Reply to" value={message.Headers["Reply-To"]} /> : null}
    </Facts></Panel>
    {message.HTMLBody ? <div><Button onClick={() => setPlain(!plain)}>{plain ? "Show formatted email" : "Show plain text"}</Button></div> : null}
    {message.HTMLBody && !plain
      ? <iframe title="Email preview" sandbox="allow-popups allow-popups-to-escape-sandbox" referrerPolicy="no-referrer" srcDoc={emailPreview(message.HTMLBody)} style={{ width: "100%", minHeight: 420, border: 0, background: "white", borderRadius: 8 }} />
      : <Panel label="Email text"><pre style={{whiteSpace:"pre-wrap", overflowWrap:"anywhere"}}>{message.TextBody}</pre></Panel>}
  </div>;
  const needle = search.trim().toLowerCase();
  const filtered = messages.filter((item) => `${item.Subject} ${item.To} ${item.from}`.toLowerCase().includes(needle));
  return <div className="os-app-stack">
    <Head title="Inbox" meta={mode === "capture" ? "Captured for testing" : undefined}>
      <RefreshButton label="Refresh inbox" busy={loading || !connection} onClick={() => { void refresh(); }} />
    </Head>
    {error ? <Notice tone="warn" sentence="Could not read email." detail={error} /> : null}
    {loading && !readAt ? <RecordListSkeleton label="Loading email" /> : null}
    {mode === "external" ? <EmptyState icon={Mail} title="Receiving is not connected">This installation sends email externally. Incoming mail will appear here when a receiving service is connected.</EmptyState> : null}
    {mode === "capture" ? <>
      <Input id="email-search" label="Search email" value={search} onChange={setSearch} placeholder="Search email" />
      {!messages.length ? <EmptyState icon={Mail} title="No test email yet">Messages sent by this installation appear here instead of being delivered externally.</EmptyState>
        : !filtered.length ? <EmptyState title="No matching email">Try another subject or address.</EmptyState>
        : <RecordList label="Captured messages">{filtered.map((item) => <RecordRow key={item.id} icon={<Mail size={16} aria-hidden />} name={item.Subject} secondary={item.To} trailing={formatMoment(item.createdAt)} onOpen={() => {setSelected(item.id); setPlain(false);}} />)}</RecordList>}
      <p className="os-muted">Last 100 messages from seven days. {readAt ? `Updated ${formatMoment(readAt)}.` : ""} This test inbox is restricted to owners and developers.</p>
    </> : null}
  </div>;
}
