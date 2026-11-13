import { useState } from "react";
import { Mail } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, EmptyState, Head, Notice, LiveList, RecordRow, Refine, Select, formatMoment } from "../../../kit";
import { LoadMore } from "../../../kit/LoadMore";
import { flatten } from "../../../kit/rows";
import { useLiveCollection, usePagedCollection } from "../../../live/useLiveCollection";
import { useLiveView } from "../../../live/liveView";
import { useDebouncedValue } from "../../../logs/useDebouncedValue";
import { useAccountOptions } from "../../accounts/tie";
import { organizationName } from "../organizations";
import { MessagePage } from "./MessagePage";

export interface CapturedEmail {
  id: string; createdAt: string; accountId?: string;
  from: string; fromName: string; To: string; Subject: string;
  TextBody: string; HTMLBody: string; Headers?: Record<string, string>;
}
type InboxRow = Partial<CapturedEmail> & { id: string; mode?: string };
const MODE_ID = "email-inbox-mode";

/** Captured mail is encrypted, so the gated native reader searches bounded
 * ciphertext pages and returns metadata. Only an open message loads its body. */
export function InboxSection() {
  const accounts = useAccountOptions();
  const [selected, setSelected] = useState("");
  const [search, setSearch] = useState("");
  const [organization, setOrganization] = useState("");
  const needle = useDebouncedValue(search.trim(), 250);
  const key = JSON.stringify([needle, organization]);
  const feed = usePagedCollection<InboxRow>(`email:inbox:${key}`, connection => ({
    concept: "v1:email:capturedMessage", actions: [],
    seed: async (cursor, signal) => {
      const result = await connection.query.emailInboxPage({ search: needle || undefined, accountId: organization || undefined, cursor: cursor || undefined }, { signal });
      const raw = result.rows()[0];
      if (!raw) throw new Error("The inbox returned no result.");
      const data = flatten(raw);
      return { rows: [{ id: MODE_ID, mode: typeof data.mode === "string" ? data.mode : "" }, ...(Array.isArray(data.messages) ? data.messages as InboxRow[] : [])], nextCursor: typeof data.nextCursor === "string" ? data.nextCursor : "" };
    },
  }));
  const detail = useLiveCollection<CapturedEmail>(selected ? `email:inbox-message:${selected}` : null, connection => ({
    concept: "v1:email:capturedMessage", actions: [], paged: false,
    seed: async (_cursor, signal) => {
      const result = await connection.query.emailInboxMessage({ messageId: selected }, { signal });
      const first = result.rows()[0];
      const message = first ? flatten(first).message : null;
      return { rows: message && typeof message === "object" ? [message as CapturedEmail] : [], nextCursor: "" };
    },
  }));
  const mode = feed.snapshot.rows.find(row => row.id === MODE_ID)?.mode;
  const source = useLiveView<InboxRow, InboxRow>(feed.source, key, rows => rows.filter(row => row.id !== MODE_ID));
  const message = detail.snapshot.rows[0];
  const error = detail.snapshot.error || feed.snapshot.error;
  const busy = feed.snapshot.state === "seeding" || feed.snapshot.state === "disconnected";
  const refresh = () => { feed.reseed(); if (selected) detail.reseed(); };
  return <div className="os-app-stack">
    <AutoRefresh resume={false} busy={busy} onRefresh={refresh} />
    {message ? <MessagePage message={message} onBack={() => setSelected("")} /> : <>
      <Head title="Inbox" meta={mode === "capture" ? "Past 7 days" : undefined}>
        {mode === "capture" ? <Refine iconOnly label="Refine inbox" search={search} onSearch={setSearch} chips={organization ? [{ id: "organization", label: organizationName(accounts, organization), onRemove: () => setOrganization("") }] : []}>
          {accounts.length > 1 ? <Select id="email-inbox-organization" label="Organization" value={organization} onChange={setOrganization}><option value="">Any organization</option>{accounts.map(account => <option key={account.id} value={account.id}>{organizationName(accounts, account.id)}</option>)}</Select> : null}
        </Refine> : null}
      </Head>
      {selected && detail.snapshot.state === "live" && !detail.snapshot.error ? <Notice tone="info" sentence="That captured message is no longer available." /> : null}
      {mode === "external" ? <EmptyState icon={Mail} title="Nothing is captured on this cluster">This cluster delivers email to real inboxes.</EmptyState> : <LiveList<InboxRow> source={source} rowId={row => row.id} fingerprint={row => `${row.Subject}:${row.To}`} label="Captured messages"
        errorContent={null}
        emptyContent={feed.hasMore ? <EmptyState title="No matches in these messages">Scroll to search older messages.</EmptyState> : <EmptyState icon={Mail} title={needle || organization ? "No matching email" : "No test email yet"}>{needle || organization ? "Try another subject or address." : "Email this cluster sends appears here instead of being delivered."}</EmptyState>}
        renderRow={item => <RecordRow icon={<Mail size={16} aria-hidden />} name={item.Subject || "(No subject)"} secondary={item.To} trailing={formatMoment(item.createdAt ?? "")} onOpen={() => setSelected(item.id)} />} />}
      <LoadMore feed={feed} />
    </>}
    {error ? <Notice tone="warn" sentence="Could not read email." detail={refusalSentence(error)}><Button busy={busy} onClick={refresh}>Try again</Button></Notice> : null}
  </div>;
}
