import type { ExecutionDestination } from "../kit/ExecutionDestination";
// Inference routing remains independent of the goal's tool destination.
export type AskLevel = "" | "fast" | "strong" | "reasoning";

export interface AskRouting {
  destination?: ExecutionDestination;
  /** The source selector, sent as AiChatMsg.provider. "" is Auto. */
  source: string;
  /** The level, sent as AiChatMsg.level. "" is Auto. */
  level: AskLevel;
}

export const AUTO_ROUTING: AskRouting = Object.freeze({ source: "", level: "" }) as AskRouting;
