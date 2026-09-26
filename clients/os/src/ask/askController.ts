import type { AskActivity, AskConversationStore } from "./conversationSession";
// Shared contract for the cluster transport and injected test transports.

import type { PolicyDraft } from "../apps/fleet/PolicyEditor";

export interface AskCallbacks {
  policyProposal?: (proposal: PolicyDraft) => void;
  activity?: (event: AskActivity) => void;
  delta: (text: string) => void;
  done: () => void;
  error: (message: string) => void;
}

export interface AskHandle {
  cancel: () => void;
}

export interface AskTransport {
 cancelGoal?: (goalId: string) => Promise<void>;
  conversations?: AskConversationStore;
  startVoice?: (options: import("@znasllc-io/memql-sdk-core/voice").AskVoiceOptions, signal: AbortSignal) => Promise<import("@znasllc-io/memql-sdk-core/voice").AskVoiceCredentials>;
  /**
   * Stream an answer. `context` is the surface's context tag
   * ("app:artifacts section:browse") or null from the desk/orb.
   */
  ask(prompt: string, context: string | null, on: AskCallbacks, options?: { conversationId: string; turnId: string }): AskHandle;
}
