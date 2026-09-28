import type { AskHandle, AskTransport } from "./askController";
import { AUTO_ROUTING, LocalAskRouteStore, isAuto, type AskRouteStore, type AskRouting } from "./askRoute";

export interface AskActivity {
  id: string;
  kind: "model" | "action" | "run" | "artifact";
  phase: "running" | "completed" | "failed" | "fallback";
  at: string;
  provider?: string;
  model?: string;
  name?: string;
  app?: string;
  navigate?: boolean;
  arguments?: Record<string, unknown>;
  elapsedMs?: number;
  expectedMs?: number;
  estimateSource?: string;
  call?: { vendor?: string; policy?: string; rule?: string; door?: string; modality?: string; executionSurface?: string; servedModel?: string; cacheKind?: string; inputTokens: number; outputTokens: number; tokensEstimated: boolean; totalCost: number; pricingConfigured: boolean; billing?: string; firstTokenMs: number };
  error?: string;
}
export interface AskTurn {
  goalId?: string;
  runId?: string;
  id: string;
  prompt: string;
  answer: string;
  state: "streaming" | "done" | "error" | "interrupted";
  startedAt: string;
  endedAt?: string;
  activity: AskActivity[];
  error?: string;
}
export interface ConversationSummary { id: string; title: string; createdAt?: string }
export interface AskConversationStore {
  list(): Promise<ConversationSummary[]>;
  create(): Promise<ConversationSummary>;
  read(id: string): Promise<AskTurn[]>;
}
export interface ConversationState {
  conversations: ConversationSummary[];
  dictationActivity: AskActivity[];
  historyLoading: boolean;
  historyError: string;
  selectedId: string | null;
  turns: AskTurn[];
  draft: string;
  busy: boolean;
  loading: boolean;
  error: string;
  activity: AskActivity | null;
  voiceActive: boolean;
  /** Where this conversation's replies are answered, and with what effort.
   *  Auto until the viewer chooses; see askRoute.ts. */
  routing: AskRouting;
}

/** One session survives closing the overlay and is observed by both surfaces.
 * Transcripts live on the cluster; no private conversation enters localStorage.
 * The one thing kept in this browser is the viewer's route choice per
 * conversation id (askRoute.ts) -- a preference, never content. */
export class ConversationSession {
  private state: ConversationState = { conversations: [], dictationActivity: [], historyLoading: false, historyError: "", selectedId: null, turns: [], draft: "", busy: false, loading: false, error: "", activity: null, voiceActive: false, routing: AUTO_ROUTING };
  private listeners = new Set<() => void>();
  private active: AskHandle | null = null;
  private stopRequested = false;
  private cancelling = false;
  private requestStarted = false;
  private epoch = 0;
  private selection = 0;
  private historyEpoch = 0;
  private voiceEpoch = 0;
  private voiceRevision = 0;
  private voiceTurnId: string | null = null;
  private pendingVoiceActivity = new Map<string, AskActivity[]>();
  constructor(private transport: AskTransport, private routes: AskRouteStore = new LocalAskRouteStore()) {}
  getSnapshot = () => this.state;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private patch(patch: Partial<ConversationState>) { this.state = { ...this.state, ...patch }; this.listeners.forEach(listener => listener()); }
  setDraft = (draft: string) => this.patch({ draft });
  /** Choose this conversation's route. It applies to the NEXT message -- a
   *  reply already streaming keeps the route it was sent with -- and it is
   *  remembered with the conversation. A conversation not yet created (the
   *  first message creates it) adopts the choice when it is. */
  setRouting = (routing: AskRouting) => {
    this.patch({ routing: { source: routing.source, level: routing.level } });
    if (this.state.selectedId) this.routes.save(this.state.selectedId, this.state.routing);
  };
  /** A conversation this session just created takes the choice made before it existed. */
  private adopt(id: string) { if (!isAuto(this.state.routing)) this.routes.save(id, this.state.routing); }
  recordDictation = (activity: AskActivity) => {
    const previous = this.state.dictationActivity.find(item => item.id === activity.id);
    const completed = this.state.dictationActivity.filter(item => item.phase === "completed" && item.provider === activity.provider && item.model === activity.model && (item.elapsedMs ?? 0) > 0).slice(-20).map(item => item.elapsedMs!).sort((a, b) => a - b);
    const expectedMs = previous?.expectedMs ?? (completed.length ? completed[Math.ceil(completed.length * .75) - 1] : activity.provider?.toLowerCase().includes("fleet") ? 60000 : 15000);
    this.patch({ dictationActivity: [...this.state.dictationActivity.slice(-199), { ...activity, expectedMs, estimateSource: previous?.estimateSource ?? (completed.length ? "Recent dictation on this route" : "Route baseline") }] });
  };
  refresh = async () => {
    if (!this.transport.conversations) return;
    const epoch = ++this.historyEpoch;
    this.patch({ historyLoading: true, historyError: "" });
    try {
      const conversations = await this.transport.conversations.list();
      if (epoch === this.historyEpoch) this.patch({ conversations, historyLoading: false });
    } catch (error) { if (epoch === this.historyEpoch) this.patch({ historyLoading: false, historyError: message(error) }); }
  };
  newConversation = () => {
    if (this.state.busy || this.state.voiceActive) return;
    this.selection++;
    this.patch({ selectedId: null, turns: [], dictationActivity: [], draft: "", error: "", loading: false, routing: AUTO_ROUTING });
  };
  select = async (id: string) => {
    if (this.state.busy || this.state.voiceActive || !this.transport.conversations) return;
    const selection = ++this.selection;
    this.patch({ loading: true, error: "" });
    try {
      const turns = await this.transport.conversations.read(id);
      if (selection === this.selection) this.patch({ selectedId: id, turns, dictationActivity: [], draft: "", loading: false, routing: this.routes.load(id) });
    } catch (error) { if (selection === this.selection) this.patch({ loading: false, error: message(error) }); }
  };
  detach = (reason = "Connection ended. The work continues in Nexus.") => {
    this.epoch++;
    this.active?.cancel(); this.active = null;
    this.patch({ busy: false, activity: null, turns: this.state.turns.map(turn => turn.state === "streaming" ? { ...turn, state: "interrupted", error: reason } : turn) });
  };
  navigationFailed = (error: string) => { this.patch({ error, activity: null }); };
  stop = () => {
    if (!this.state.busy) return;
    this.stopRequested = true;
    if (!this.requestStarted) { this.detach("Stop requested."); return; }
    void this.cancelCurrentGoal();
  };
  private async cancelCurrentGoal() {
    if (!this.stopRequested || this.cancelling) return;
    const goalId = [...this.state.turns].reverse().find(turn => turn.state === "streaming")?.goalId;
    if (!goalId) return; // Intake's durable run receipt is still in flight.
    if (!this.transport.cancelGoal) { this.patch({ error: "This connection cannot cancel work." }); return; }
    const epoch = this.epoch;
    this.cancelling = true;
    try {
      await this.transport.cancelGoal(goalId);
      if (epoch === this.epoch) this.detach("Stop requested.");
    } catch (error) { if (epoch === this.epoch) { this.stopRequested = false; this.patch({ error: `Could not stop work: ${message(error)}` }); } }
    finally { this.cancelling = false; }
  }
  dispose = () => { this.detach(); this.selection++; this.historyEpoch++; this.voiceEpoch++; this.listeners.clear(); };
  beginVoice = async (): Promise<string> => {
    if (this.state.busy || this.state.loading || this.state.voiceActive) throw new Error("Finish the current turn first.");
    const epoch = ++this.voiceEpoch;
    this.patch({ voiceActive: true, error: "" });
    try {
      if (this.state.selectedId) return this.state.selectedId;
      if (!this.transport.conversations) throw new Error("Conversations are unavailable.");
      const item = await this.transport.conversations.create();
      if (epoch !== this.voiceEpoch) throw new Error("Voice start was cancelled.");
      this.adopt(item.id);
      this.patch({ selectedId: item.id, conversations: [item, ...this.state.conversations], turns: [] });
      return item.id;
    } catch (error) { if (epoch === this.voiceEpoch) this.patch({ voiceActive: false }); throw error; }
  };
  endVoice = () => { this.voiceEpoch++; this.voiceRevision++; this.voiceTurnId = null; this.pendingVoiceActivity.clear(); this.patch({ voiceActive: false, busy: false, activity: null }); void this.reload(); };
  reload = async () => {
    const id = this.state.selectedId;
    const selection = this.selection;
    const revision = this.voiceRevision;
    if (!id || !this.transport.conversations) return;
    try { const turns = await this.transport.conversations.read(id); if (this.state.selectedId === id && selection === this.selection && revision === this.voiceRevision && !this.state.busy) this.patch({ turns }); }
    catch (error) { if (selection === this.selection && revision === this.voiceRevision) this.patch({ error: message(error) }); }
    void this.refresh();
  };
  voiceEvent = (event: { type: string; turnId?: string; text?: string; error?: string; activity?: AskActivity }) => {
    if (!this.state.voiceActive || !event.turnId) return;
    this.voiceRevision++;
    const id = event.turnId;
    const existing = this.state.turns.find(turn => turn.id === id);
    if (event.type === "transcript") {
      if (existing) return;
      this.voiceTurnId = id;
      const activity = this.pendingVoiceActivity.get(id) ?? []; this.pendingVoiceActivity.delete(id);
      this.patch({ busy: true, turns: [...this.state.turns, { id, prompt: event.text ?? "", answer: "", state: "streaming", startedAt: new Date().toISOString(), activity }] });
    } else if (event.type === "text" && existing) {
      this.patch({ turns: this.state.turns.map(turn => turn.id === id ? { ...turn, answer: turn.answer + event.text } : turn) });
    } else if (event.type === "activity" && event.activity) {
      const activity = event.activity;
      if (!existing) this.pendingVoiceActivity.set(id, [...(this.pendingVoiceActivity.get(id) ?? []), activity]);
      this.patch({ turns: this.state.turns.map(turn => turn.id === id ? { ...turn, activity: [...turn.activity, activity], ...runIdentity(activity) } : turn), ...(activity.kind === "action" && activity.navigate ? { activity } : {}) });
    } else if (event.type === "done") {
      this.patch({ busy: this.voiceTurnId === id ? false : this.state.busy, turns: this.state.turns.map(turn => turn.id === id ? { ...turn, state: turn.error ? "error" : "done", endedAt: new Date().toISOString() } : turn) });
      void this.reload();
    } else if (event.type === "error") {
      this.patch({ error: event.error ?? "Voice turn failed", turns: this.state.turns.map(turn => turn.id === id ? { ...turn, state: "error", error: event.error } : turn) });
    }
  };
  send = (prompt: string, context: string | null): boolean => {
    if (this.state.busy || this.state.voiceActive || this.state.loading || !prompt.trim()) return false;
    const epoch = ++this.epoch;
    this.stopRequested = false; this.requestStarted = false;
    const turn: AskTurn = { id: crypto.randomUUID(), prompt: prompt.trim(), answer: "", state: "streaming", startedAt: new Date().toISOString(), activity: [] };
    // Captured at SEND time: a choice made while this reply streams applies
    // to the next message, never to this one.
    const routing = this.state.routing;
    this.patch({ busy: true, draft: "", error: "", turns: [...this.state.turns, turn] });
    const patchTurn = (patch: Partial<AskTurn>) => {
      if (epoch !== this.epoch) return;
      Object.assign(turn, patch);
      this.patch({ turns: this.state.turns.map(item => item.id === turn.id ? { ...turn } : item) });
    };
    const finish = (error?: string) => {
      if (epoch !== this.epoch) return;
      patchTurn({ state: error ? "error" : "done", error, endedAt: new Date().toISOString() });
      this.epoch++;
      this.active = null;
      this.patch({ busy: false });
      void this.refresh();
    };
    void (async () => {
      try {
        let id = this.state.selectedId;
        if (!id && this.transport.conversations) {
          const conversation = await this.transport.conversations.create();
          if (epoch !== this.epoch) return;
          id = conversation.id;
          this.adopt(id);
          this.patch({ selectedId: id, conversations: [conversation, ...this.state.conversations] });
        }
        if (epoch !== this.epoch) return;
        this.requestStarted = true;
        const handle = this.transport.ask(turn.prompt, context, {
          delta: text => patchTurn({ answer: turn.answer + text }),
          activity: event => {
            if (epoch !== this.epoch) return;
            patchTurn({ activity: [...turn.activity, event], ...runIdentity(event) });
            if (event.kind === "action" && event.navigate) this.patch({ activity: event });
            if (event.kind === "run" && this.stopRequested) void this.cancelCurrentGoal();
          },
          done: () => finish(turn.answer.trim() ? undefined : "MemQL finished without an answer."),
          error: error => finish(error),
        }, id ? { conversationId: id, turnId: turn.id, routing } : undefined);
        if (epoch === this.epoch) this.active = handle;
      } catch (error) { finish(message(error)); }
    })();
    return true;
  };
}

function message(error: unknown) { return error instanceof Error ? error.message : String(error); }

function runIdentity(event: AskActivity): Partial<AskTurn> {
  if (event.kind !== "run") return {};
  return { goalId: typeof event.arguments?.goalId === "string" ? event.arguments.goalId : undefined, runId: typeof event.arguments?.runId === "string" ? event.arguments.runId : undefined };
}
