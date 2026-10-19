import type { AskHandle, AskTransport } from "./askController";
import { AUTO_ROUTING, routeIdFromSource, routeSource, type AskRouting } from "./askRoute";
import type { ExecutionDestination } from "../kit/ExecutionDestination";
import type { UploadHandle, UploadProvider } from "../items/upload";

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
/** A Library file attached to a sent message, as the transcript records it. */
export interface AskTurnAttachment { artifactId: string; fileId?: string; name: string; mimeType?: string; kind?: string }
/** A file in the message field, before the message is sent. */
export interface AskAttachmentDraft {
  /** Local id; never sent. */
  id: string;
  name: string;
  size: number;
  state: "uploading" | "ready" | "failed";
  /** 0..1 while uploading, from the upload's own byte count. */
  progress?: number;
  artifactId?: string;
  fileId?: string;
  /** The upload's refusal, verbatim, when it failed. */
  error?: string;
}
/** A message carries at most this many files; the cluster holds the same bound. */
export const MAX_ASK_ATTACHMENTS = 10;
/** The route and Where a message is sent with. "" is Auto. */
export interface AskRoutingChoice { route: string; destination: ExecutionDestination }

/** One goal a message went to, and what became of it there (`routeLabel`). */
export interface AskRouteTarget {
  goalId: string;
  runId: string;
  /** The goal's current title; the cluster re-reads it on every snapshot. */
  title: string;
  status: "queued" | "received" | "answered" | "notApproved" | "saved" | "notDelivered" | "notReceived";
  /** Present when the message answered one of the goal's requests rather than guiding its work. */
  approvalId?: string;
  reason?: "ended" | "review" | "limit" | "refused";
  /** Exactly what the goal received. */
  text: string;
}
/** The person asked to drop a goal; nothing stops until they confirm, on its card or by answering the reply's question. */
export interface AskStopQuestion {
  goalId: string;
  runId: string;
  title: string;
  /** "" while it waits on the person; "ended" when the goal ended unanswered. */
  decision?: "" | "stopped" | "kept" | "ended";
}
/** Where a message went when it reached goals already running in the conversation. */
export interface AskRouted {
  targets?: AskRouteTarget[];
  stops?: AskStopQuestion[];
  /** Said before a split request's own answer. */
  reply?: string;
  /** What intake received when it was not the message as written: a split
   *  message's new request, or a settled question's two messages. */
  statement?: string;
  clarify?: boolean;
  resolved?: boolean;
  skipped?: string;
}

export interface AskTurn {
  /** What the turn ran with: absent for Auto, recorded by the cluster. */
  route?: { source?: string; level?: string; destination?: string };
  attachments?: AskTurnAttachment[];
  /** Absent for a message that only opened its own goal. */
  routed?: AskRouted;
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
export interface AskQuestion { id: string; text: string; kind: "text" | "choice" | "multi"; options: { value: string; label: string }[]; optional?: boolean; continued?: boolean }
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
  destination?: ExecutionDestination;
  /** The message field's route: "" is Auto, otherwise a route id. */
  route?: string;
  /** Files in the message field, in the order they were added. */
  attachments?: AskAttachmentDraft[];
  /** Stop questions being answered now, by `stopKey`. */
  stopDecisions?: Readonly<Record<string, "stop" | "keep">>;
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
 * The message field's route and Where ride each turn (Auto sends nothing), and
 * an open conversation keeps the choice its own last message ran with. */
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
  /** A refused stop decision, still said while its question waits on the person. */
  private stopRefusal: { key: string; error: string } | null = null;
  /** Moves when a stop decision is sent and when it is recorded, so a read that
   *  left before either cannot put back a question the person answered. */
  private stopRevision = 0;
  private voiceEpoch = 0;
  private voiceRevision = 0;
  private voiceTurnId: string | null = null;
  private pendingVoiceActivity = new Map<string, AskActivity[]>();
  private routingDefault: AskRoutingChoice = { route: "", destination: "auto" };
  private routingListener: ((choice: AskRoutingChoice) => void) | null = null;
  private uploads = new Map<string, UploadHandle>();
  constructor(private transport: AskTransport) {}
  getSnapshot = () => this.state;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private patch(patch: Partial<ConversationState>) { this.state = { ...this.state, ...patch }; this.listeners.forEach(listener => listener()); }
  setDraft = (draft: string) => this.patch({ draft });
  setDestination = (destination: ExecutionDestination) => { this.patch({ destination }); this.emitRouting(); };
  /** A route choice, and with it the Where it implies (Apps runs on the
   *  person's own computer). Auto is "". */
  setRoute = (route: string, destination?: ExecutionDestination) => {
    this.patch({ route: route === "auto" ? "" : route, ...(destination ? { destination } : {}) });
    this.emitRouting();
  };
  /** What a new conversation starts from: the person's last choice. Applied
   *  at once while nothing has been said in this session yet. */
  setRoutingDefault = (choice: AskRoutingChoice) => {
    this.routingDefault = { route: choice.route === "auto" ? "" : choice.route, destination: choice.destination };
    if (this.state.turns.length === 0 && this.state.route === undefined) this.patch({ ...this.routingDefault });
  };
  /** Told every time the person changes the route or Where, to remember it. */
  onRoutingChoice = (listener: ((choice: AskRoutingChoice) => void) | null) => { this.routingListener = listener; };
  private emitRouting() { this.routingListener?.({ route: this.state.route ?? "", destination: this.state.destination ?? "auto" }); }
  /** A conversation's own choice: what its last ordinary message ran with, or
   *  the person's default for one with no messages yet. */
  private routingFor(turns: readonly AskTurn[]): AskRoutingChoice {
    const last = [...turns].reverse().find(turn => !turn.approvalId);
    if (!last) return { ...this.routingDefault };
    const destination = last.route?.destination;
    return { route: routeIdFromSource(last.route?.source), destination: destination === "workbench" || destination === "computer" ? destination : "auto" };
  }
  /** Upload files into the person's Library and hold them in the field. */
  attach = (files: readonly File[], uploads: UploadProvider) => {
    const held = this.state.attachments ?? [];
    const room = Math.max(0, MAX_ASK_ATTACHMENTS - held.length);
    const accepted = files.slice(0, room);
    const drafts: AskAttachmentDraft[] = accepted.map(file => ({ id: crypto.randomUUID(), name: file.name, size: file.size, state: "uploading", progress: 0 }));
    this.patch({ attachments: [...held, ...drafts], ...(files.length > accepted.length ? { error: `A message can carry at most ${MAX_ASK_ATTACHMENTS} files.` } : {}) });
    drafts.forEach((draft, index) => {
      const handle = uploads.upload(accepted[index]!);
      this.uploads.set(draft.id, handle);
      const stop = handle.onProgress?.(progress => this.patchAttachment(draft.id, { progress: progress.totalBytes > 0 ? Math.min(1, progress.sentBytes / progress.totalBytes) : 0 }));
      handle.done.then(
        result => this.patchAttachment(draft.id, { state: "ready", progress: 1, artifactId: result.artifactId, ...(result.fileId ? { fileId: result.fileId } : {}) }),
        error => this.patchAttachment(draft.id, { state: "failed", error: message(error) }),
      ).finally(() => { stop?.(); this.uploads.delete(draft.id); });
    });
  };
  /** Take a file out of the message. An upload still running is stopped; a
   *  file that already landed stays in the Library. */
  removeAttachment = (id: string) => {
    this.uploads.get(id)?.abort();
    this.uploads.delete(id);
    this.patch({ attachments: (this.state.attachments ?? []).filter(item => item.id !== id) });
  };
  /** Put a failed turn's files back in the field for another try. */
  restoreAttachments = (attachments: readonly AskTurnAttachment[] | undefined) => {
    if (!attachments?.length) return;
    const held = this.state.attachments ?? [];
    const back = attachments.filter(file => !held.some(item => item.artifactId === file.artifactId)).map(file => ({ id: crypto.randomUUID(), name: file.name, size: 0, state: "ready" as const, progress: 1, artifactId: file.artifactId, ...(file.fileId ? { fileId: file.fileId } : {}) }));
    this.patch({ attachments: [...held, ...back].slice(0, MAX_ASK_ATTACHMENTS) });
  };
  private patchAttachment(id: string, patch: Partial<AskAttachmentDraft>) {
    const held = this.state.attachments ?? [];
    if (!held.some(item => item.id === id)) return;
    this.patch({ attachments: held.map(item => item.id === id ? { ...item, ...patch } : item) });
  }
  questionFocused = () => this.patch({ focusQuestionId: null });
  /** Answer the stop question one message asked about a goal (`AskStopQuestion`).
   *  Stop closes the goal; Keep only records the answer. The card's question
   *  goes once the cluster has recorded it, and a refusal is said in the
   *  surface's error line with the question still there to answer again. */
  decideStop = async (turnId: string, runId: string, decision: "stop" | "keep"): Promise<boolean> => {
    const conversationId = this.state.selectedId;
    const key = stopKey(turnId, runId);
    if (!conversationId || this.disposed || this.state.stopDecisions?.[key]) return false;
    if (!this.transport.decideStop) { this.patch({ error: "Stopping work is unavailable." }); return false; }
    const selection = this.selection;
    const current = () => !this.disposed && selection === this.selection && this.state.selectedId === conversationId;
    this.stopRefusal = null;
    this.stopRevision++;
    this.patch({ stopDecisions: { ...this.state.stopDecisions, [key]: decision }, error: "" });
    try {
      await this.transport.decideStop({ conversationId, turnId, runId, decision });
      if (!current()) return true;
      // Recorded: the question is answered even if the read that follows fails,
      // and a read that left while it was being recorded answers too early.
      this.stopRevision++;
      // The answer is about the goal, so it settles every question still
      // asking about it, as the cluster records it: none comes back.
      const answered = decision === "stop" ? "stopped" : "kept";
      this.patch({ turns: this.state.turns.map(turn => !turn.routed?.stops?.some(stop => stop.runId === runId && !stop.decision) ? turn : { ...turn, routed: { ...turn.routed, stops: turn.routed.stops.map(stop => stop.runId === runId && !stop.decision ? { ...stop, decision: answered } : stop) } }) });
      await this.reload();
      return true;
    } catch (error) {
      if (current()) {
        this.stopRefusal = { key, error: message(error) };
        this.patch({ error: this.stopRefusal.error });
      }
      return false;
    } finally {
      if (this.state.stopDecisions?.[key]) {
        const rest = { ...this.state.stopDecisions };
        delete rest[key];
        this.patch({ stopDecisions: rest });
      }
    }
  };
  discussApproval = async (approvalId: string) => {
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
    if (!turn?.approvalId || !turn.error || this.state.busy || this.state.voiceActive || this.state.loading || this.state.discussionSending || this.state.guidanceSending) return false;
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
    this.patch({ selectedId: null, openingId: null, selectionError: "", turns: [], dictationActivity: [], draft: "", error: "", snapshotError: "", loading: false, focusQuestionId: null, ...this.routingFor([]) });
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
    if (id === this.state.selectedId && !this.state.openingId && !this.state.loading) return true;
    if (this.state.busy || this.state.voiceActive || !this.transport.conversations) return false;
    if (!forDiscussion) this.leaveDiscussion();
    const selection = ++this.selection;
    this.patch({ loading: true, openingId: id, selectionError: "", error: "", snapshotError: "", focusQuestionId: null });
    try {
      const turns = await this.transport.conversations.read(id);
      if (selection === this.selection) {
        this.patch({ discussion: null });
        this.patch({ selectedId: id, openingId: null, turns, dictationActivity: [], draft: "", loading: false, ...this.routingFor(turns) });
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
    this.scheduleRefresh();
  };
  dispose = () => {
    this.disposed = true;
    for (const upload of this.uploads.values()) upload.abort();
    this.uploads.clear();
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this.refreshTimer = null;
    this.detach();
    this.selection++; this.historyEpoch++; this.voiceEpoch++; this.discussionEpoch++;
    this.listeners.clear();
  };
  beginVoice = async (): Promise<string> => {
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
    if (this.disposed || !this.transport.conversations || !this.state.turns.some(turn => isBackgroundTurn(turn) || turn.state === "streaming")) return;
    this.refreshTimer = setTimeout(() => { this.refreshTimer = null; void this.reload(false); }, this.state.error || this.state.snapshotError ? 10000 : this.state.turns.some(turn => isBackgroundTurn(turn) && turn.state !== "waiting") ? 2500 : 10000);
  }
  reload = (refreshHistory = true): Promise<void> => {
    const id = this.state.selectedId;
    const selection = this.selection;
    const epoch = this.epoch;
    const discussionEpoch = this.discussionEpoch;
    const revision = this.voiceRevision;
    const guidanceRevision = this.guidanceRevision;
    const stopRevision = this.stopRevision;
    if (!id || !this.transport.conversations || this.disposed || this.state.loading || this.state.discussionOpening) return Promise.resolve();
    const key = JSON.stringify([id, selection, epoch, discussionEpoch, revision, guidanceRevision, stopRevision]);
    if (this.reloadRequest?.key === key) {
      this.reloadRequest.refreshHistory ||= refreshHistory;
      return this.reloadRequest.promise;
    }
    const request = { key, refreshHistory, promise: Promise.resolve() };
    this.reloadRequest = request;
    const current = () => !this.disposed && this.state.selectedId === id && selection === this.selection && epoch === this.epoch && discussionEpoch === this.discussionEpoch && revision === this.voiceRevision && guidanceRevision === this.guidanceRevision && stopRevision === this.stopRevision;
    request.promise = (async () => {
      try { const turns = await this.transport.conversations!.read(id); if (current()) {
        // A stream can lose its first run event as well as its terminal event.
        // The accepted client request ID in this conversation correlates the
        // receipt even before a run ID arrived. A known run must still match.
        // A message that only went to goals already running opens no run of
        // its own: its finished copy says where it went (`routed`) instead.
        const local = new Map(this.state.turns.filter(turn => turn.state === "streaming").map(turn => [turn.id, turn]));
        let recoveredStream = false;
        const merged = turns.map(turn => {
          const active = local.get(turn.id);
          const correlated = turn.runId ? !active?.runId || active.runId === turn.runId : Boolean(turn.routed) && !active?.runId;
          if (!this.state.voiceActive && active && correlated && (turn.state === "done" || turn.state === "error")) {
            local.delete(turn.id);
            recoveredStream = true;
            return turn;
          }
          return active ?? turn;
        });
        for (const turn of local.values()) if (!merged.some(item => item.id === turn.id)) merged.push(turn);
        if (recoveredStream && !this.state.voiceActive) {
          this.epoch++; // Late stream callbacks cannot overwrite the receipt.
          this.active?.cancel(); this.active = null;
        }
        // Read recovery must not retain an old snapshot error or erase an
        // uncertain guidance/discussion write and its retry receipt, nor a
        // refused stop while its question still waits on the person.
        const refusal = this.stopRefusal;
        const refused = refusal !== null && this.state.error === refusal.error && [...pendingStops(merged)].some(([runId, stop]) => stopKey(stop.turnId, runId) === refusal.key);
        if (!refused) this.stopRefusal = null;
        this.patch({ turns: merged, snapshotError: "", ...(recoveredStream && !this.state.voiceActive ? { busy: false } : {}), ...(this.state.discussion || refused ? {} : { error: "" }) });
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
    if (this.state.discussionOpening) return false;
    if (this.state.discussionDraft) {
      if (this.state.discussionDraft.approvalId !== this.state.discussion?.approval.id) return false;
      return this.sendDiscussion(prompt, this.state.discussionDraft.advisor);
    }
    if (this.state.discussion) return this.sendDiscussion(prompt);
    if (this.state.busy || this.state.voiceActive || this.state.loading || !prompt.trim()) return false;
    const held = this.state.attachments ?? [];
    // A file still uploading has no artifact yet; the message waits for it.
    if (held.some(item => item.state === "uploading")) return false;
    const attached = held.filter(item => item.state === "ready" && item.artifactId).map(item => ({ artifactId: item.artifactId!, name: item.name, ...(item.fileId ? { fileId: item.fileId } : {}) }));
    const epoch = ++this.epoch;
    this.stopRequested = false; this.requestStarted = false;
    const turn: AskTurn = { id: crypto.randomUUID(), prompt: prompt.trim(), answer: "", state: "streaming", startedAt: new Date().toISOString(), activity: [], ...(attached.length ? { attachments: attached } : {}) };
    const routing = askRouting(this.state.route ?? "", this.state.destination);
    if (routing !== AUTO_ROUTING) turn.route = { ...(routing.source ? { source: routing.source } : {}), ...(routing.destination ? { destination: routing.destination } : {}) };
    this.patch({ busy: true, draft: "", error: "", turns: [...this.state.turns, turn], attachments: [] });
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
      // A message that went only to work already running opens no run of its
      // own. The cluster saved where it went before it said done, so read it
      // now: its labels and any stop question arrive with the reply, not a
      // poll later.
      if (!error && !turn.runId) void this.reload(false);
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
            if (turn.runId && !this.refreshTimer) this.scheduleRefresh();
            if (event.kind === "action" && event.navigate) this.patch({ activity: event });
            if (event.kind === "run" && this.stopRequested) void this.cancelCurrentGoal();
          },
          done: () => finish(turn.answer.trim() || isBackgroundTurn(turn) ? undefined : "MemQL finished without an answer."),
          error: error => finish(error),
        }, { conversationId: id ?? "", turnId: turn.id, routing, ...(attached.length ? { attachments: attached.map(item => item.artifactId) } : {}) });
        if (epoch === this.epoch) { this.active = handle; this.scheduleRefresh(); }
      } catch (error) { finish(message(error)); }
    })();
    return true;
  };
}

function message(error: unknown) { return error instanceof Error ? error.message : String(error); }

/** What one message is sent with. Auto for both is the frozen AUTO_ROUTING,
 *  so an unrouted message sends exactly what it always did. */
export function askRouting(route: string, destination: ExecutionDestination | undefined): AskRouting {
  const source = routeSource(route);
  const where = destination && destination !== "auto" ? destination : undefined;
  if (!source && !where) return AUTO_ROUTING;
  return { ...AUTO_ROUTING, source, ...(where ? { destination: where } : {}) };
}

export function isBackgroundTurn(turn: AskTurn) { return Boolean(turn.runId) && (turn.state === "queued" || turn.state === "waiting" || turn.state === "interrupted"); }
/** A recovered unclassified/quick request stays inline while observation resumes. */
export function isConversationalWait(turn: AskTurn) { return isBackgroundTurn(turn) && turn.state !== "waiting" && !turn.background && !turn.acknowledgement && (!turn.workload || turn.workload === "quick"); }

/** One stop question's identity: the message that asked it and the goal's run. */
export function stopKey(turnId: string, runId: string) { return JSON.stringify([turnId, runId]); }

/** The stop question still waiting on the person for each goal, by the goal's
 *  run. A later message's question about the same goal is the one asked. */
export function pendingStops(turns: readonly AskTurn[]): Map<string, { turnId: string; question: AskStopQuestion }> {
  const pending = new Map<string, { turnId: string; question: AskStopQuestion }>();
  for (const turn of turns) {
    for (const question of turn.routed?.stops ?? []) {
      if (question.runId && !question.decision) pending.set(question.runId, { turnId: turn.id, question });
    }
  }
  return pending;
}

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
