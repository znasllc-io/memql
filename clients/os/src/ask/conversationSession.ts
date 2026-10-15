import type { AskHandle, AskTransport, WorkGuidanceRequest } from "./askController";
import { AUTO_ROUTING } from "./askRoute";

export interface AskActivity {
  id: string;
  kind: "model" | "action" | "run" | "artifact";
  phase: "running" | "queued" | "completed" | "failed" | "fallback" | "waiting" | "cancelled";
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
  call?: { promptName?: string; purpose?: string; attempt?: number; vendor?: string; policy?: string; rule?: string; door?: string; modality?: string; executionSurface?: string; servedModel?: string; cacheKind?: string; inputTokens: number; outputTokens: number; tokensEstimated: boolean; totalCost: number; pricingConfigured: boolean; billing?: string; firstTokenMs: number };
  error?: string;
}
export interface AskTurn {
  canGuideWork?: boolean;
  guidanceEpoch?: string;
  guidanceTargetRunId?: string;
  guidanceStatus?: "queued" | "applied" | "not-received";
  approvalId?: string;
  advisor?: boolean;
  question?: AskQuestion;
  answerOnly?: boolean;
  goalId?: string;
  runId?: string;
  acknowledgement?: string;
  acknowledgedAt?: string;
  workStatus?: string;
  background?: boolean;
  workTitle?: string;
  workload?: string;
  id: string;
  prompt: string;
  answer: string;
  state: "streaming" | "queued" | "waiting" | "done" | "error" | "interrupted";
  startedAt: string;
  endedAt?: string;
  activity: AskActivity[];
  error?: string;
}
export interface AskQuestion { id: string; text: string; kind: "text" | "choice" | "multi"; options: { value: string; label: string }[] }
export interface ConversationSummary { id: string; title: string; createdAt?: string }
export interface ApprovalDiscussion {
  conversationId: string;
  approval: { id: string; runId: string; kind: string; question: string; summary?: string; status: string };
  canSendGuidance: boolean;
  canAskAdvisor: boolean;
  guidanceMode?: "resume" | "deferred";
  savedGuidance?: string;
  guidanceToResume?: string;
}
export interface AskConversationStore {
  list(): Promise<ConversationSummary[]>;
  create(): Promise<ConversationSummary>;
  read(id: string): Promise<AskTurn[]>;
}
export interface ConversationState {
  workGuidance?: { targetEpoch: string; runId: string; conversationId: string; title: string } | null;
  workGuidanceSending?: boolean;
  workGuidanceRetry?: WorkGuidanceRequest | null;
  discussion?: ApprovalDiscussion | null;
  discussionOpening?: boolean;
  discussionSending?: boolean;
  discussionRetry?: { approvalId: string; prompt: string; advisor: boolean } | null;
  discussionDraft?: { approvalId: string; advisor: boolean } | null;
  guidanceSending?: boolean;
  guidanceNotice?: string;
  guidanceRetry?: { approvalId: string; feedback: string } | null;
  focusQuestionId?: string | null;
  conversations: ConversationSummary[];
  dictationActivity: AskActivity[];
  historyLoading: boolean;
  historyError: string;
  selectionError: string;
  openingId: string | null;
  selectedId: string | null;
  turns: AskTurn[];
  draft: string;
  busy: boolean;
  loading: boolean;
  error: string;
  snapshotError: string;
  activity: AskActivity | null;
  voiceActive: boolean;
}

/** One session survives closing the overlay and is observed by both surfaces.
 * Transcripts live on the cluster; no private conversation enters localStorage.
 * Routing stays automatic; deliberate overrides belong to run controls. */
export class ConversationSession {
  private state: ConversationState = { conversations: [], dictationActivity: [], historyLoading: false, historyError: "", selectionError: "", openingId: null, selectedId: null, turns: [], draft: "", busy: false, loading: false, error: "", snapshotError: "", activity: null, voiceActive: false };
  private listeners = new Set<() => void>();
  private active: AskHandle | null = null;
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;
  private reloadRequest: { key: string; refreshHistory: boolean; promise: Promise<void> } | null = null;
  private disposed = false;
  private stopRequested = false;
  private cancelling = false;
  private requestStarted = false;
  private epoch = 0;
  private selection = 0;
  private historyEpoch = 0;
  private discussionEpoch = 0;
  private guidanceRevision = 0;
  private uncertainGuidance = new Map<string, string>();
  private pendingDiscussionRequest: { key: string; id: string } | null = null;
  private workGuidanceEpoch = 0;
  private workGuidanceRevision = 0;
  private pendingWorkGuidance = new Map<string, WorkGuidanceRequest>();
  private voiceEpoch = 0;
  private voiceRevision = 0;
  private voiceTurnId: string | null = null;
  private pendingVoiceActivity = new Map<string, AskActivity[]>();
  constructor(private transport: AskTransport) {}
  getSnapshot = () => this.state;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private patch(patch: Partial<ConversationState>) { this.state = { ...this.state, ...patch }; this.listeners.forEach(listener => listener()); }
  setDraft = (draft: string) => this.patch({ draft });
  questionFocused = () => this.patch({ focusQuestionId: null });
  beginWorkGuidance = (turnId: string): boolean => {
    const turn = this.state.turns.find(item => item.id === turnId);
    if (!turn || !canGuideTurn(turn) || !this.state.selectedId || !this.transport.guideWork || this.disposed || this.state.busy || this.state.loading || this.state.voiceActive || this.state.discussion || this.state.discussionDraft || this.state.discussionOpening || this.state.workGuidanceSending) return false;
    this.workGuidanceEpoch++;
    const target = { targetEpoch: turn.guidanceEpoch!, runId: turn.runId!, conversationId: this.state.selectedId, title: turn.workTitle || turn.prompt };
    const retry = this.pendingWorkGuidance.get(JSON.stringify([target.conversationId, target.runId]));
    this.patch({ workGuidance: target, workGuidanceSending: false, workGuidanceRetry: retry ?? null, error: "" });
    return true;
  };
  leaveWorkGuidance = () => {
    this.workGuidanceEpoch++;
    this.patch({ workGuidance: null, workGuidanceSending: false, workGuidanceRetry: null, error: "" });
  };
  retryWorkGuidance = () => {
    const retry = this.state.workGuidanceRetry;
    if (!retry || retry.runId !== this.state.workGuidance?.runId || retry.conversationId !== this.state.selectedId) return false;
    return this.submitWorkGuidance(retry, false);
  };
  private sendWorkGuidance = (prompt: string): boolean => {
    const target = this.state.workGuidance;
    if (!target || target.conversationId !== this.state.selectedId || this.state.workGuidanceRetry || !prompt.trim()) return false;
    if (!this.state.turns.some(turn => turn.runId === target.runId && turn.guidanceEpoch === target.targetEpoch && canGuideTurn(turn))) {
      this.patch({ error: "This work changed or is no longer available for guidance. Select it again, or return to conversation to start a new request." }); return false;
    }
    if (Array.from(prompt.trim()).length > 4000) { this.patch({ error: "Keep guidance to 4,000 characters or fewer." }); return false; }
    return this.submitWorkGuidance({ targetEpoch: target.targetEpoch, runId: target.runId, conversationId: target.conversationId, requestId: crypto.randomUUID(), prompt: prompt.trim() }, true);
  };
  private submitWorkGuidance = (request: WorkGuidanceRequest, clearDraft: boolean): boolean => {
    if (!this.transport.guideWork || this.disposed || this.state.workGuidanceSending || this.state.loading || this.state.busy || this.state.voiceActive) return false;
    const epoch = this.workGuidanceEpoch;
    const key = JSON.stringify([request.conversationId, request.runId]);
    this.pendingWorkGuidance.set(key, request);
    this.workGuidanceRevision++;
    this.patch({ workGuidanceSending: true, workGuidanceRetry: null, error: "", ...(clearDraft ? { draft: "" } : {}) });
    void this.transport.guideWork(request).then(async receipt => {
      if (receipt.runId !== request.runId || receipt.conversationId !== request.conversationId || receipt.requestId !== request.requestId || !receipt.turn?.id || receipt.turn.guidanceTargetRunId !== request.runId || receipt.turn.prompt !== request.prompt || receipt.turn.answerOnly !== true || receipt.turn.runId || receipt.turn.goalId || receipt.turn.guidanceStatus !== receipt.status || !["queued", "applied", "not-received"].includes(receipt.status)) throw new Error("The cluster did not confirm this guidance. Retry to check the original message.");
      const pending = this.pendingWorkGuidance.get(key);
      if (pending?.requestId === request.requestId && pending.targetEpoch === request.targetEpoch) this.pendingWorkGuidance.delete(key);
      if (epoch !== this.workGuidanceEpoch || this.disposed) return;
      this.workGuidanceRevision++;
      const turn = receipt.turn;
      this.patch({ turns: this.state.turns.some(item => item.id === turn.id) ? this.state.turns : [...this.state.turns, turn] });
      await this.reload();
    }).catch((error: unknown) => {
      if (epoch === this.workGuidanceEpoch && !this.disposed) this.patch({ error: message(error), workGuidanceRetry: request, ...(clearDraft && this.state.draft === "" ? { draft: request.prompt } : {}) });
    }).finally(() => {
      if (epoch === this.workGuidanceEpoch && !this.disposed) this.patch({ workGuidanceSending: false });
    });
    return true;
  };
  discussApproval = async (approvalId: string) => {
    if (this.state.workGuidance) { this.patch({ error: "Return to conversation before opening an approval discussion." }); return false; }
    if (this.state.discussionSending || this.state.guidanceSending) { this.patch({ error: "Wait for the current message to be recorded before opening another approval." }); return false; }
    if (this.state.busy || this.state.voiceActive) { this.patch({ error: "Finish the current reply before opening an approval discussion." }); return false; }
    const epoch = ++this.discussionEpoch;
    const before = this.selection;
    this.patch({ discussionOpening: true, discussionDraft: this.state.discussionDraft?.approvalId === approvalId ? this.state.discussionDraft : null, error: "" });
    try {
      if (!this.transport.discussApproval) throw new Error("Approval discussion is unavailable.");
      const discussion = await this.transport.discussApproval(approvalId);
      if (epoch !== this.discussionEpoch || before !== this.selection || this.disposed) return false;
      if (discussion.approval.id !== approvalId) throw new Error("The approval discussion did not match the selected request.");
      // select is a no-op for the already-open conversation, preserving its draft.
      if (!await this.select(discussion.conversationId, true)) return false;
      if (epoch !== this.discussionEpoch || this.disposed) return false;
      this.patch({ discussion, guidanceRetry: this.guidanceReceipt(discussion), discussionOpening: false, guidanceNotice: "" });
      await this.reload();
      return epoch === this.discussionEpoch && !this.disposed;
    } catch (error) {
      if (epoch === this.discussionEpoch && before === this.selection) this.patch({ error: message(error) });
    } finally {
      if (epoch === this.discussionEpoch) this.patch({ discussionOpening: false });
    }
    return false;
  };
  prepareApprovalTurnRetry = async (turnId: string): Promise<boolean> => {
    const turn = this.state.turns.find(item => item.id === turnId);
    if (!turn?.approvalId || !turn.error || this.state.workGuidance || this.state.busy || this.state.voiceActive || this.state.loading || this.state.discussionSending || this.state.guidanceSending) return false;
    const selected = this.state.selectedId;
    const intent = { approvalId: turn.approvalId, advisor: turn.advisor === true };
    // Keep the read-only intent even if reopening fails. Only deliberately
    // leaving discussion may turn this composer back into ordinary execution.
    this.patch({ discussionDraft: intent, discussion: null });
    const opened = await this.discussApproval(turn.approvalId);
    return opened && this.state.selectedId === selected && this.state.discussionDraft === intent && this.state.discussion?.approval.id === intent.approvalId && this.state.discussion.canAskAdvisor;
  };
  leaveDiscussion = () => {
    this.discussionEpoch++;
    this.patch({ discussion: null, discussionOpening: false, discussionSending: false, discussionRetry: null, discussionDraft: null, guidanceSending: false, guidanceNotice: "", guidanceRetry: null });
  };
  private guidanceReceipt(discussion: ApprovalDiscussion) {
    const approvalId = discussion.approval.id;
    if (discussion.guidanceMode === "deferred") {
      const feedback = discussion.canSendGuidance ? this.uncertainGuidance.get(approvalId) : undefined;
      // Saving budget guidance leaves the approval pending. A fresh read of
      // the exact saved text resolves a lost response; it is never a resume.
      if (!feedback || discussion.savedGuidance === feedback) {
        this.uncertainGuidance.delete(approvalId);
        return null;
      }
      return { approvalId, feedback };
    }
    // A saved receipt is authoritative after reopening. An uncertain local
    // send can only retry its original text while the request is still open.
    const feedback = discussion.guidanceToResume || (discussion.canSendGuidance ? this.uncertainGuidance.get(approvalId) : undefined);
    if (!feedback) this.uncertainGuidance.delete(approvalId);
    return feedback ? { approvalId, feedback } : null;
  }
  sendGuidance = async () => {
    const discussion = this.state.discussion;
    const feedback = this.state.draft.trim();
    if (!discussion?.canSendGuidance || this.state.guidanceRetry || !feedback) return false;
    if (Array.from(feedback).length > 4000) { this.patch({ error: "Keep guidance to 4,000 characters or fewer." }); return false; }
    return this.submitGuidance(discussion.approval.id, feedback, true);
  };
  retryGuidance = async () => {
    const receipt = this.state.guidanceRetry;
    if (!receipt || receipt.approvalId !== this.state.discussion?.approval.id) return false;
    return this.submitGuidance(receipt.approvalId, receipt.feedback, false);
  };
  private submitGuidance = async (approvalId: string, feedback: string, clearSubmittedDraft: boolean) => {
    if (this.state.guidanceSending || this.state.discussionOpening || this.state.discussionSending) return false;
    const epoch = this.discussionEpoch;
    const deferred = this.state.discussion?.guidanceMode === "deferred";
    this.guidanceRevision++;
    this.uncertainGuidance.set(approvalId, feedback);
    this.patch({ guidanceSending: true, error: "" });
    try {
      if (!this.transport.approvalGuidance) throw new Error("Sending guidance is unavailable.");
      // The same approval and exact original answer are the retry identity.
      const receipt = await this.transport.approvalGuidance(approvalId, feedback);
      if (deferred && (receipt.guidanceSaved !== true || receipt.savedGuidance !== feedback || receipt.runResumed)) {
        throw new Error("The cluster did not confirm this saved guidance. Retry saving to check the original text.");
      }
      this.uncertainGuidance.delete(approvalId);
      if (epoch !== this.discussionEpoch || this.disposed) return true;
      this.guidanceRevision++;
      const discussion = deferred
        ? { ...this.state.discussion!, savedGuidance: receipt.savedGuidance, guidanceToResume: undefined }
        : { ...this.state.discussion!, canSendGuidance: false, guidanceToResume: receipt.runResumed ? undefined : feedback };
      this.patch({ discussion, guidanceRetry: this.guidanceReceipt(discussion), guidanceNotice: deferred ? "" : receipt.runResumed ? "Guidance sent. Work is continuing." : `Guidance was recorded, but the work has not resumed.${receipt.resumeError ? ` ${receipt.resumeError}` : ""}`, ...(clearSubmittedDraft && this.state.draft.trim() === feedback ? { draft: "" } : {}) });
      await this.reload();
      return true;
    } catch (error) {
      if (epoch === this.discussionEpoch) this.patch({ error: message(error), guidanceRetry: { approvalId, feedback } });
      return false;
    } finally {
      if (epoch === this.discussionEpoch) this.patch({ guidanceSending: false });
    }
  };
  sendDiscussion = (prompt: string, advisor = false): boolean => this.submitDiscussion(prompt, advisor, true);
  retryDiscussion = (): boolean => {
    const receipt = this.state.discussionRetry;
    if (!receipt || receipt.approvalId !== this.state.discussion?.approval.id) return false;
    return this.submitDiscussion(receipt.prompt, receipt.advisor, false);
  };
  private submitDiscussion = (prompt: string, advisor: boolean, clearSubmittedDraft: boolean): boolean => {
    const discussion = this.state.discussion;
    if (!discussion || this.state.discussionOpening || this.state.discussionSending || this.state.guidanceSending || !prompt.trim()) return false;
    if (!this.transport.approvalMessage) { this.patch({ error: "Approval discussion is unavailable." }); return false; }
    const epoch = this.discussionEpoch;
    const request = { approvalId: discussion.approval.id, conversationId: discussion.conversationId, prompt: prompt.trim(), advisor };
    const key = JSON.stringify(request);
    const retry = this.pendingDiscussionRequest?.key === key;
    if ((!discussion.canAskAdvisor || this.state.discussionRetry) && !retry) return false;
    const requestId = retry ? this.pendingDiscussionRequest!.id : crypto.randomUUID();
    this.pendingDiscussionRequest = { key, id: requestId };
    this.patch({ discussionSending: true, discussionRetry: null, ...(clearSubmittedDraft ? { draft: "" } : {}), error: "" });
    void this.transport.approvalMessage({ ...request, requestId }).then(async receipt => {
      if (epoch !== this.discussionEpoch || this.disposed) return;
      this.pendingDiscussionRequest = null;
      const turn: AskTurn = { id: receipt.turnId, runId: receipt.runId, prompt: request.prompt, answer: "", state: "queued", startedAt: new Date().toISOString(), activity: [], background: true, approvalId: request.approvalId, advisor };
      this.patch({ turns: this.state.turns.some(item => item.id === turn.id) ? this.state.turns : [...this.state.turns, turn] });
      await this.reload();
    }).catch((error: unknown) => {
      if (epoch === this.discussionEpoch) this.patch({ error: message(error), discussionRetry: { approvalId: request.approvalId, prompt: request.prompt, advisor }, ...(clearSubmittedDraft && this.state.draft === "" ? { draft: prompt } : {}) });
    }).finally(() => {
      if (epoch === this.discussionEpoch) this.patch({ discussionSending: false });
    });
    return true;
  };
  openWork = async (runId: string) => {
    if (this.state.busy || this.state.voiceActive) { this.patch({ error: "Finish the current reply before opening another conversation." }); return; }
    const before = this.selection;
    try {
      if (!this.transport.openWork) throw new Error("Opening work in Ask is unavailable.");
      const conversation = await this.transport.openWork(runId);
      if (before !== this.selection || this.disposed) return;
      if (await this.select(conversation.id)) {
        const selected = this.selection;
        await this.reload();
        if (selected !== this.selection || this.state.selectedId !== conversation.id) return;
        this.patch({ focusQuestionId: this.state.turns.find(turn => turn.runId === runId && turn.question)?.question?.id ?? null });
      }
    } catch (error) { this.patch({ error: message(error) }); }
  };
  answerQuestion = async (question: AskQuestion, answer: Record<string, unknown>) => {
    if (!this.transport.answerQuestion) throw new Error("Answering questions is unavailable.");
    await this.transport.answerQuestion(question.id, answer);
    await this.reload(false);
  };
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
    this.leaveDiscussion();
    this.leaveWorkGuidance();
    this.patch({ selectedId: null, openingId: null, selectionError: "", turns: [], dictationActivity: [], draft: "", error: "", snapshotError: "", loading: false, focusQuestionId: null });
  };
  /** The explicit New action persists even when no inference is available. */
  createConversation = async () => {
    if (this.disposed || this.state.busy || this.state.loading || this.state.voiceActive) return;
    const selection = ++this.selection;
    this.patch({ loading: true, error: "" });
    try {
      if (!this.transport.conversations) throw new Error("Conversations are unavailable.");
      const item = await this.transport.conversations.create();
      if (selection !== this.selection || this.disposed) return;
      this.historyEpoch++;
      this.newConversation();
      this.patch({ selectedId: item.id, conversations: [item, ...this.state.conversations.filter(row => row.id !== item.id)], historyLoading: false });
    } catch (error) {
      if (selection === this.selection && !this.disposed) this.patch({ loading: false, error: message(error) });
    }
  };
  select = async (id: string, forDiscussion = false) => {
    if (id === this.state.selectedId && !this.state.openingId) return true;
    if (this.state.busy || this.state.voiceActive || !this.transport.conversations) return false;
    if (!forDiscussion) this.leaveDiscussion();
    const selection = ++this.selection;
    this.patch({ loading: true, openingId: id, selectionError: "", error: "", snapshotError: "", focusQuestionId: null });
    try {
      const turns = await this.transport.conversations.read(id);
      if (selection === this.selection) {
        this.leaveWorkGuidance();
        this.patch({ discussion: null });
        this.patch({ selectedId: id, openingId: null, turns, dictationActivity: [], draft: "", loading: false });
        this.scheduleRefresh();
        return true;
      }
    } catch (error) {
      if (selection === this.selection) {
        this.patch({ loading: false, openingId: null, selectionError: message(error) });
      }
    }
    return false;
  };
  /** Leaving the list cancels a pending selection, never the current reply. */
  cancelSelection = () => {
    this.selection++;
    this.patch({ loading: false, openingId: null, selectionError: "" });
  };
  detach = (reason = "Connection ended. The work continues in Nexus.") => {
    this.epoch++;
    this.active?.cancel(); this.active = null;
    this.patch({ busy: false, activity: null, turns: this.state.turns.map(turn => turn.state === "streaming" ? { ...turn, state: "interrupted", error: reason } : turn) });
    this.scheduleRefresh();
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
  activate = () => {
    this.disposed = false;
    const target = this.state.workGuidance;
    if (target && this.state.workGuidanceSending) this.patch({ workGuidanceSending: false, workGuidanceRetry: this.pendingWorkGuidance.get(JSON.stringify([target.conversationId, target.runId])) ?? null });
    this.scheduleRefresh();
  };
  dispose = () => {
    this.disposed = true;
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this.refreshTimer = null;
    this.detach();
    this.selection++; this.historyEpoch++; this.voiceEpoch++; this.discussionEpoch++;
    this.workGuidanceEpoch++;
    this.listeners.clear();
  };
  beginVoice = async (): Promise<string> => {
    if (this.state.workGuidance) throw new Error("Use typing or dictation while guiding work.");
    if (this.state.discussion || this.state.discussionOpening || this.state.discussionDraft) throw new Error("Use typing or dictation while discussing an approval.");
    if (this.state.busy || this.state.loading || this.state.voiceActive) throw new Error("Finish the current turn first.");
    const epoch = ++this.voiceEpoch;
    this.patch({ voiceActive: true, error: "" });
    try {
      if (this.state.selectedId) return this.state.selectedId;
      if (!this.transport.conversations) throw new Error("Conversations are unavailable.");
      const item = await this.transport.conversations.create();
      if (epoch !== this.voiceEpoch) throw new Error("Voice start was cancelled.");
      this.patch({ selectedId: item.id, conversations: [item, ...this.state.conversations], turns: [] });
      return item.id;
    } catch (error) { if (epoch === this.voiceEpoch) this.patch({ voiceActive: false }); throw error; }
  };
  endVoice = () => { this.voiceEpoch++; this.voiceRevision++; this.voiceTurnId = null; this.pendingVoiceActivity.clear(); this.patch({ voiceActive: false, busy: false, activity: null }); void this.reload(); };
  private scheduleRefresh() {
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this.refreshTimer = null;
    if (this.disposed || !this.state.turns.some(isBackgroundTurn)) return;
    this.refreshTimer = setTimeout(() => { this.refreshTimer = null; void this.reload(false); }, this.state.error || this.state.snapshotError ? 10000 : this.state.turns.some(turn => isBackgroundTurn(turn) && turn.state !== "waiting") ? 2500 : 10000);
  }
  reload = (refreshHistory = true): Promise<void> => {
    const id = this.state.selectedId;
    const selection = this.selection;
    const epoch = this.epoch;
    const discussionEpoch = this.discussionEpoch;
    const revision = this.voiceRevision;
    const guidanceRevision = this.guidanceRevision;
    const workGuidanceRevision = this.workGuidanceRevision;
    if (!id || !this.transport.conversations || this.disposed || this.state.loading || this.state.discussionOpening) return Promise.resolve();
    const key = JSON.stringify([id, selection, epoch, discussionEpoch, revision, guidanceRevision, workGuidanceRevision]);
    if (this.reloadRequest?.key === key) {
      this.reloadRequest.refreshHistory ||= refreshHistory;
      return this.reloadRequest.promise;
    }
    const request = { key, refreshHistory, promise: Promise.resolve() };
    this.reloadRequest = request;
    const current = () => !this.disposed && this.state.selectedId === id && selection === this.selection && epoch === this.epoch && discussionEpoch === this.discussionEpoch && revision === this.voiceRevision && guidanceRevision === this.guidanceRevision && workGuidanceRevision === this.workGuidanceRevision;
    request.promise = (async () => {
      try { const turns = await this.transport.conversations!.read(id); if (current()) {
        // A background completion may arrive while a newer foreground reply is
        // streaming. Preserve that local stream and any not-yet-accepted turn.
        const local = new Map(this.state.turns.filter(turn => turn.state === "streaming").map(turn => [turn.id, turn]));
        const merged = turns.map(turn => local.get(turn.id) ?? turn);
        for (const turn of local.values()) if (!merged.some(item => item.id === turn.id)) merged.push(turn);
        // Read recovery must not retain an old snapshot error or erase an
        // uncertain guidance/discussion write and its retry receipt.
        this.patch({ turns: merged, snapshotError: "", ...(this.state.discussion || this.state.workGuidance ? {} : { error: "" }) });
        const discussion = this.state.discussion;
        if (discussion && this.transport.discussApproval) {
          const latest = await this.transport.discussApproval(discussion.approval.id);
          if (current()) {
            const recoveredSave = latest.guidanceMode === "deferred" && !!this.state.guidanceRetry && this.state.guidanceRetry.feedback === latest.savedGuidance;
            this.patch({ discussion: latest, guidanceRetry: this.guidanceReceipt(latest), ...(recoveredSave ? { error: "" } : {}) });
          }
        }
      } }
      catch (error) { if (current()) this.patch({ snapshotError: message(error) }); }
      if (request.refreshHistory && current()) void this.refresh();
      this.scheduleRefresh();
    })().finally(() => { if (this.reloadRequest === request) this.reloadRequest = null; });
    return request.promise;
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
      this.patch({ turns: this.state.turns.map(turn => turn.id === id ? { ...turn, activity: [...turn.activity, activity], ...runIdentity(activity), ...backgroundReceipt(activity) } : turn), ...(activity.kind === "action" && activity.navigate ? { activity } : {}) });
    } else if (event.type === "done") {
      this.patch({ busy: this.voiceTurnId === id ? false : this.state.busy, turns: this.state.turns.map(turn => turn.id === id ? { ...turn, state: turn.error ? "error" : isBackgroundTurn(turn) ? turn.state : "done", ...(isBackgroundTurn(turn) ? { acknowledgement: turn.answer } : { endedAt: new Date().toISOString() }) } : turn) });
      void this.reload();
    } else if (event.type === "error") {
      this.patch({ error: event.error ?? "Voice turn failed", turns: this.state.turns.map(turn => turn.id === id ? { ...turn, state: "error", error: event.error } : turn) });
    }
  };
  send = (prompt: string, context: string | null): boolean => {
    if (this.state.workGuidance) return this.sendWorkGuidance(prompt);
    if (this.state.discussionOpening) return false;
    if (this.state.discussionDraft) {
      if (this.state.discussionDraft.approvalId !== this.state.discussion?.approval.id) return false;
      return this.sendDiscussion(prompt, this.state.discussionDraft.advisor);
    }
    if (this.state.discussion) return this.sendDiscussion(prompt);
    if (this.state.busy || this.state.voiceActive || this.state.loading || !prompt.trim()) return false;
    const epoch = ++this.epoch;
    this.stopRequested = false; this.requestStarted = false;
    const turn: AskTurn = { id: crypto.randomUUID(), prompt: prompt.trim(), answer: "", state: "streaming", startedAt: new Date().toISOString(), activity: [] };
    const routing = AUTO_ROUTING;
    this.patch({ busy: true, draft: "", error: "", turns: [...this.state.turns, turn] });
    const patchTurn = (patch: Partial<AskTurn>) => {
      if (epoch !== this.epoch) return;
      Object.assign(turn, patch);
      this.patch({ turns: this.state.turns.map(item => item.id === turn.id ? { ...turn } : item) });
    };
    const finish = (error?: string) => {
      if (epoch !== this.epoch) return;
      const background = isBackgroundTurn(turn);
      const observing = background || Boolean(error && turn.runId);
      patchTurn({ state: error ? turn.runId ? "interrupted" : "error" : background ? turn.state : "done", error, endedAt: observing ? undefined : new Date().toISOString(), ...(background && !error ? { acknowledgement: turn.answer } : {}) });
      this.epoch++;
      this.active = null;
      this.patch({ busy: false });
      this.scheduleRefresh();
      void this.refresh();
    };
    void (async () => {
      try {
        let id = this.state.selectedId;
        if (!id && this.transport.conversations) {
          const conversation = await this.transport.conversations.create();
          if (epoch !== this.epoch) return;
          id = conversation.id;
          this.patch({ selectedId: id, conversations: [conversation, ...this.state.conversations] });
        }
        if (epoch !== this.epoch) return;
        this.requestStarted = true;
        const handle = this.transport.ask(turn.prompt, context, {
          delta: text => patchTurn({ answer: turn.answer + text }),
          activity: event => {
            if (epoch !== this.epoch) return;
            patchTurn({ activity: [...turn.activity, event], ...runIdentity(event), ...backgroundReceipt(event) });
            if (event.kind === "action" && event.navigate) this.patch({ activity: event });
            if (event.kind === "run" && this.stopRequested) void this.cancelCurrentGoal();
          },
          done: () => finish(turn.answer.trim() || isBackgroundTurn(turn) ? undefined : "MemQL finished without an answer."),
          error: error => finish(error),
        }, id ? { conversationId: id, turnId: turn.id, routing } : undefined);
        if (epoch === this.epoch) this.active = handle;
      } catch (error) { finish(message(error)); }
    })();
    return true;
  };
}

function message(error: unknown) { return error instanceof Error ? error.message : String(error); }

export function isBackgroundTurn(turn: AskTurn) { return Boolean(turn.runId) && (turn.state === "queued" || turn.state === "waiting" || turn.state === "interrupted"); }
export function canGuideTurn(turn: AskTurn) { return turn.canGuideWork === true && !!turn.guidanceEpoch && !turn.approvalId && !turn.answerOnly && isBackgroundTurn(turn); }

function runIdentity(event: AskActivity): Partial<AskTurn> {
  if (event.kind !== "run") return {};
  return { ...(typeof event.arguments?.goalId === "string" ? { goalId: event.arguments.goalId } : {}), ...(typeof event.arguments?.runId === "string" ? { runId: event.arguments.runId } : {}) };
}

function backgroundReceipt(event: AskActivity): Partial<AskTurn> {
  if (event.kind !== "run" || !(event.arguments?.background === true || event.phase === "queued" || event.phase === "waiting")) return {};
  const fields: Partial<AskTurn> = { state: event.phase === "waiting" ? "waiting" : "queued", background: true };
  for (const key of ["workTitle", "workload", "workStatus", "acknowledgedAt"] as const) {
    if (typeof event.arguments?.[key] === "string") fields[key] = event.arguments[key];
  }
  return fields;
}
