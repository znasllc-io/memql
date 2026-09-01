// Ask's transcription wire (epic memql#4747): sdk-core `pushToTalk` behind
// the same `VoiceTranscriber` seam the session's tests fill with a fake.
//
// The declared format is the one micCapture actually produces. It is not a
// preference: openai-realtime, the cluster default, ignores `format` and
// resamples whatever arrives as though it were 16 kHz PCM16, so declaring
// anything else here is a lie the server cannot catch. See ask/pcm16.ts.

import { pushToTalk } from "@znasllc-io/memql-sdk-core/voice";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";

import type { VoiceTranscriber } from "./voiceSession";

export type PushToTalkFn = typeof pushToTalk;

export class SdkTranscriber implements VoiceTranscriber {
  constructor(
    private readonly dispatcher: () => Dispatcher | null,
    private readonly send: PushToTalkFn = pushToTalk,
  ) {}

  async run(
    audio: ReadableStream<Uint8Array>,
    opts: { sampleRate: number; onPartial: (text: string) => void; signal: AbortSignal },
  ): Promise<string> {
    const dispatcher = this.dispatcher();
    if (!dispatcher) {
      // The same sentence the chat transport uses for the same condition, so
      // the two halves of Ask do not describe one cluster two ways.
      throw new Error("Not connected to the cluster yet.");
    }
    const final = await this.send(dispatcher, audio, {
      audio: { encoding: "pcm16", sampleRate: opts.sampleRate, channels: 1 },
      onPartial: (partial) => opts.onPartial(partial.text),
      signal: opts.signal,
    });
    return final.text;
  }
}
