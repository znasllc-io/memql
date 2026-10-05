import { useEffect, useId, useState, useSyncExternalStore } from "react";
import { Head, Input, Notice, RecordListSkeleton, formatMoment } from "../kit";
import { ActionBar } from "../kit/ActionBar";
import { RecordList, RecordRow } from "../kit/RecordRow";
import { InlineSkeleton } from "../kit/ContentSkeleton";
import type { ConversationSession } from "./conversationSession";

export function AskConversations({ conversation, onOpen }: { conversation: ConversationSession; onOpen: (changed: boolean) => void }) {
  const state = useSyncExternalStore(conversation.subscribe, conversation.getSnapshot);
  const [filter, setFilter] = useState("");
  const [failedId, setFailedId] = useState<string | null>(null);
  const searchId = useId();
  useEffect(() => () => conversation.cancelSelection(), [conversation]);
  const items = state.conversations.filter(item => item.title.toLocaleLowerCase().includes(filter.trim().toLocaleLowerCase()));
  const error = state.selectionError || state.historyError;

  async function open(id: string) {
    const changed = id !== state.selectedId;
    setFailedId(id);
    if (await conversation.select(id)) onOpen(changed);
  }

  return <section className="os-ask-conversations" aria-label="Conversations" tabIndex={-1}>
    <div className="os-ask-conversations-body">
      <Head title="Conversations" navigation={false} meta={state.historyLoading && state.conversations.length ? <InlineSkeleton label="Refreshing conversations" /> : undefined} />
      {state.conversations.length ? <div className="os-ask-conversations-search"><Input id={searchId} label="Find a conversation" placeholder="Find a conversation…" value={filter} onChange={setFilter} /></div> : null}
      {error ? <Notice tone="error" sentence={error} /> : null}
      {state.historyLoading && !state.conversations.length ? <RecordListSkeleton label="Loading conversations" rows={3} /> : null}
      {!state.historyLoading && !state.historyError && !state.conversations.length ? <p className="os-caption">No conversations yet.</p> : null}
      {state.conversations.length && !items.length ? <p className="os-caption">No conversations match.</p> : null}
      <RecordList as="ul" label="Saved conversations" density="compact">
        {items.map(item => <RecordRow key={item.id} name={<span title={item.title}>{item.title || "New conversation"}</span>}
          label={item.title || "New conversation"}
          secondary={item.createdAt ? <time dateTime={item.createdAt}>{formatMoment(item.createdAt)}</time> : undefined}
          current={item.id === state.selectedId} state={item.id === state.openingId ? "Opening…" : item.id === state.selectedId ? "Current" : undefined}
          disabled={state.loading || ((state.busy || state.voiceActive) && item.id !== state.selectedId)} onOpen={() => { void open(item.id); }} />)}
      </RecordList>
    </div>
    <ActionBar state={state.loading ? "Opening conversation…" : state.busy ? "Reply in progress" : state.voiceActive ? "Voice conversation in progress" : ""}
      acts={error ? [{ label: "Retry", onAct: () => { if (state.selectionError && failedId) void open(failedId); else void conversation.refresh(); } }]
        : state.busy ? [{ label: "Stop", ariaLabel: "Stop reply", onAct: conversation.stop }] : []} />
  </section>;
}
