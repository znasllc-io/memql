import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload, type AskVoiceStartPayload } from "../client/wire.js";
export type AskVoiceOptions = Omit<AskVoiceStartPayload, "requestId">;
export interface AskVoiceCredentials { url: string; token: string; room: string }
export async function startAskVoice(dispatcher: Dispatcher, options: AskVoiceOptions, signal?: AbortSignal): Promise<AskVoiceCredentials> {
 const reply = readServerPayload(await dispatcher.sendAndWait({askVoiceStart: {...options,requestId:newShortId()}},signal));
 if(reply?.kind === "queryError") throw new Error(reply.value.error?.message ?? "Could not start voice");
 if(reply?.kind !== "askVoiceStartResult") throw new Error("Unexpected voice response");
 return reply.value;
}
