// Ask uses automatic per-step Fleet routing. The transport still accepts
// explicit API choices; deliberate UI overrides belong to Nexus run controls.
export type AskLevel = "" | "fast" | "strong" | "reasoning";

export interface AskRouting {
  /** The source selector, sent as AiChatMsg.provider. "" is Auto. */
  source: string;
  /** The level, sent as AiChatMsg.level. "" is Auto. */
  level: AskLevel;
}

export const AUTO_ROUTING: AskRouting = Object.freeze({ source: "", level: "" }) as AskRouting;
