import type { ApprovalDiscussion, AskActivity, AskConversationStore } from "./conversationSession";
import type { AskRouteOption, AskRouting } from "./askRoute";
// Shared contract for the cluster transport and injected test transports.

import type { RouteProposal } from "../apps/fleet/routing/routes";

export interface AskCallbacks {
  policyProposal?: (proposal: RouteProposal) => void;
  activity?: (event: AskActivity) => void;
  delta: (text: string) => void;
  done: () => void;
  error: (message: string) => void;
  /** Observation was lost; reconcile the accepted request, never resubmit. */
  interrupted?: () => void;
}

export interface AskHandle {
  cancel: () => void;
}

export interface AskOptions {
  conversationId: string;
  turnId: string;
  /** The conversation's route: `source` rides AiChatMsg.provider and `level`
   *  AiChatMsg.level. Empty values are Auto -- the cluster's rules decide. */
  routing?: AskRouting;
  /** Bare Library artifact ids attached to this message; the cluster
   *  resolves each under the person. Absent or empty when none. */
  attachments?: string[];
}

export interface ApprovalGuidanceReceipt { runResumed: boolean; runStopped?: boolean; resumeError?: string }

export interface AskTransport {
  discussApproval?: (approvalId: string) => Promise<ApprovalDiscussion>;
  /** Answer the stop question a message asked about one goal (`askStopDecision`).
   *  Resolves once the cluster has recorded the answer; stop has closed the goal. */
  decideStop?: (request: { conversationId: string; turnId: string; runId: string; decision: "stop" | "keep" }) => Promise<void>;
  approvalMessage?: (request: { approvalId: string; conversationId: string; requestId: string; prompt: string; advisor?: boolean }) => Promise<{ conversationId: string; turnId: string; runId: string; status: string }>;
  approvalGuidance?: (approvalId: string, feedback: string) => Promise<ApprovalGuidanceReceipt>;
  openWork?: (runId: string) => Promise<import("./conversationSession").ConversationSummary>;
  answerQuestion?: (approvalId: string, answer: Record<string, unknown>) => Promise<void>;
 cancelGoal?: (goalId: string) => Promise<void>;
  conversations?: AskConversationStore;
  /** The routes this person can pick for a message (`routesForActor`).
   *  Absent on a transport that cannot ask; the picker then offers Auto alone. */
  routes?: () => Promise<AskRouteOption[]>;
  startVoice?: (options: import("@znasllc-io/memql-sdk-core/voice").AskVoiceOptions, signal: AbortSignal) => Promise<import("@znasllc-io/memql-sdk-core/voice").AskVoiceCredentials>;
  /**
   * Stream an answer. `context` is the surface's context tag
   * ("app:artifacts section:browse") or null from the desk/orb.
   */
  ask(prompt: string, context: string | null, on: AskCallbacks, options?: AskOptions): AskHandle;
}
