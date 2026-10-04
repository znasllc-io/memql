import type { AskActivity, AskConversationStore } from "./conversationSession";
import type { AskRouting } from "./askRoute";
// Shared contract for the cluster transport and injected test transports.

import type { RouteProposal } from "../apps/fleet/routing/routes";

export interface AskCallbacks {
  policyProposal?: (proposal: RouteProposal) => void;
  activity?: (event: AskActivity) => void;
  delta: (text: string) => void;
  done: () => void;
  error: (message: string) => void;
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
}

export interface AskTransport {
 cancelGoal?: (goalId: string) => Promise<void>;
  conversations?: AskConversationStore;
  startVoice?: (options: import("@znasllc-io/memql-sdk-core/voice").AskVoiceOptions, signal: AbortSignal) => Promise<import("@znasllc-io/memql-sdk-core/voice").AskVoiceCredentials>;
  /**
   * Stream an answer. `context` is the surface's context tag
   * ("app:artifacts section:browse") or null from the desk/orb.
   */
  ask(prompt: string, context: string | null, on: AskCallbacks, options?: AskOptions): AskHandle;
}
