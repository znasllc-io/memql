import { isTransportInterruption } from "@znasllc-io/memql-sdk-core/client";
import type { AskConversationStore, ConversationSummary } from "./conversationSession";

export interface ConversationHistoryState {
  conversations: ConversationSummary[];
  historySearch: string;
  historyLoading: boolean;
  historyLoadingMore: boolean;
  historyHasMore: boolean;
  historyError: string;
  historyMoreError: string;
}
export const emptyConversationHistory: ConversationHistoryState = { conversations: [], historySearch: "", historyLoading: false, historyLoadingMore: false, historyHasMore: false, historyError: "", historyMoreError: "" };
interface Entry {
  rows: ConversationSummary[];
  cursor: string;
  loaded: boolean;
  dirty: boolean;
  error: string;
  moreError: string;
  request?: { controller: AbortController; more: boolean; promise: Promise<void> };
}

/** Browser-session history, scoped to the owning ConversationSession/transport.
 * Only visible history panels retain reads. Closing the last panel cancels work,
 * while completed pages and searches remain available without another request. */
export class ConversationHistory {
  private entries = new Map<string, Entry>();
  private search = "";
  private consumers = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  constructor(private store: AskConversationStore | undefined, private changed: (state: ConversationHistoryState) => void) {}
  private entry(): Entry {
    let value = this.entries.get(this.search);
    if (!value) { value = { rows: [], cursor: "", loaded: false, dirty: false, error: "", moreError: "" }; this.entries.set(this.search, value); }
    return value;
  }
  private publish() {
    const entry = this.entry();
    this.changed({ conversations: entry.rows, historySearch: this.search, historyLoading: !!entry.request && !entry.request.more,
      historyLoadingMore: !!entry.request?.more, historyHasMore: !!entry.cursor, historyError: entry.error, historyMoreError: entry.moreError });
  }
  retain = () => {
    this.consumers++;
    const entry = this.entry();
    if (!entry.loaded || entry.dirty) void this.refresh();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      if (--this.consumers === 0) {
        clearTimeout(this.timer); this.timer = undefined;
        for (const value of this.entries.values()) this.cancel(value);
        this.publish();
      }
    };
  };
  private cancel(entry: Entry) {
    if (!entry.request) return;
    entry.dirty ||= !entry.request.more;
    entry.request.controller.abort(); entry.request = undefined;
  }
  setSearch = (value: string) => {
    const search = value.trim().toLocaleLowerCase();
    if (search === this.search) return;
    this.cancel(this.entry());
    this.search = search;
    this.publish();
    if (this.consumers && (!this.entry().loaded || this.entry().dirty)) void this.refresh();
  };
  /** Recovery and completed turns invalidate silently when history is closed.
   * A burst shares one timer and in-flight request; a later event schedules at
   * most one further read, preserving events that arrive during the snapshot. */
  invalidate = () => {
    for (const entry of this.entries.values()) entry.dirty = true;
    if (!this.consumers || this.timer !== undefined || this.entry().request) return;
    this.timer = setTimeout(() => { this.timer = undefined; void this.refresh(); }, 100);
  };
  add = (item: ConversationSummary) => {
    for (const [search, entry] of this.entries) {
      this.cancel(entry);
      if (!search || item.title.toLocaleLowerCase().includes(search)) entry.rows = [item, ...entry.rows.filter(row => row.id !== item.id)];
      entry.dirty = true;
    }
    // Explicit New should be visible even before history's first read.
    const entry = this.entry();
    if (!this.search || item.title.toLocaleLowerCase().includes(this.search)) entry.rows = [item, ...entry.rows.filter(row => row.id !== item.id)];
    this.publish();
  };
  refresh = () => this.read(false);
  loadMore = () => this.read(true);
  private read(more: boolean): Promise<void> {
    const store = this.store;
    if (!store || !this.consumers) return Promise.resolve();
    const entry = this.entry();
    if (entry.request) return entry.request.promise;
    if (more && !entry.cursor) return Promise.resolve();
    clearTimeout(this.timer); this.timer = undefined;
    const search = this.search;
    const controller = new AbortController();
    const request = { controller, more, promise: Promise.resolve() };
    entry.request = request; entry.error = ""; entry.moreError = ""; entry.dirty = false;
    this.publish();
    request.promise = (async () => {
      let failed = false;
      try {
        let rows: ConversationSummary[] = [];
        let cursor = more ? entry.cursor : "";
        const pageCount = more ? 1 : Math.max(1, Math.ceil(entry.rows.length / 10));
        let pages = 0;
        const seen = new Set<string>();
        do {
          if (cursor) seen.add(cursor);
          const page = store.page
            ? await store.page({ search: search || undefined, cursor: cursor || undefined, signal: controller.signal })
            : { rows: (await store.list()).filter(row => row.title.toLocaleLowerCase().includes(search)), cursor: "" };
          if (controller.signal.aborted || entry.request !== request) return;
          rows.push(...page.rows);
          pages++;
          cursor = page.cursor ?? "";
          if (cursor && seen.has(cursor)) throw new Error("Conversation history could not advance. Try again.");
        } while (cursor && pages < pageCount);
        const unique = new Map<string, ConversationSummary>();
        for (const row of [...(more ? entry.rows : []), ...rows]) unique.set(row.id, row);
        entry.rows = [...unique.values()]; entry.cursor = cursor; entry.loaded = true;
      } catch (error) {
        if (controller.signal.aborted || entry.request !== request) return;
        failed = true;
        const text = isTransportInterruption(error) ? "" : error instanceof Error ? error.message : String(error);
        if (more) entry.moreError = text; else { entry.error = text; entry.dirty = true; }
      } finally {
        if (entry.request !== request) return;
        entry.request = undefined;
        if (search === this.search) {
          this.publish();
          if (!failed && entry.dirty && this.consumers && this.timer === undefined) this.timer = setTimeout(() => { this.timer = undefined; void this.refresh(); }, 100);
        }
      }
    })();
    return request.promise;
  }
  dispose = () => {
    clearTimeout(this.timer); this.timer = undefined;
    for (const entry of this.entries.values()) this.cancel(entry);
    this.entries.clear(); this.search = "";
  };
}
