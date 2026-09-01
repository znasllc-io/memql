// Opening the microphone, and the honest names for every way that fails.
//
// The browser half of Ask voice (epic memql#4747): getUserMedia -> an
// AudioContext -> the pcm16 worklet -> a ReadableStream of 16 kHz PCM16 bytes,
// which is exactly what sdk-core's pushToTalk pumps. Everything with a DOM
// dependency is behind `MicPorts`, so the session state machine beside this
// file is tested with no audio stack at all.

import { CAPTURE_BLOCK_FRAMES, Pcm16Downsampler, TARGET_SAMPLE_RATE } from "./pcm16";

/** Where the module lands in the bundle root. See public/pcm16-worklet.js. */
export const PCM16_WORKLET_PATH = "pcm16-worklet.js";
export const PCM16_PROCESSOR_NAME = "memql-pcm16-tap";

/**
 * Why the microphone is not available. A REASON, not a message: the surface
 * owns the sentence, and two of these are ordinary answers rather than faults.
 *
 * `denied` covers a genuine refusal AND a Permissions-Policy that forbids the
 * page from asking, because the browser reports them identically -- Chrome
 * rejects with NotAllowedError before it prompts. That ambiguity is why
 * component/edge answers `microphone=(self)`: the header being wrong would
 * otherwise present, forever and in every cluster, as everyone having said no.
 */
export type MicBlockReason = "denied" | "no-device" | "device-busy" | "unsupported";

export class MicError extends Error {
  constructor(
    readonly reason: MicBlockReason,
    message: string,
  ) {
    super(message);
    this.name = "MicError";
  }
}

/** The DOM surface this file touches, injectable so tests need none of it. */
export interface MicPorts {
  getUserMedia(constraints: MediaStreamConstraints): Promise<MediaStream>;
  createContext(): AudioContext;
  workletPath: string;
}

export interface MicCapture {
  /** 16 kHz mono PCM16, ready for pushToTalk. Ends when `stop()` is called. */
  readonly audio: ReadableStream<Uint8Array>;
  /** Always TARGET_SAMPLE_RATE -- the graph's own rate is converted away. */
  readonly sampleRate: number;
  /** Smoothed 0..1 input level. Read on a frame clock, never in React state. */
  level(): number;
  /** Release the device and end `audio`. Idempotent. */
  stop(): Promise<void>;
}

export const browserMicPorts: MicPorts = {
  getUserMedia: (constraints) => {
    const media = globalThis.navigator?.mediaDevices;
    if (!media?.getUserMedia) {
      throw new MicError("unsupported", "This browser has no microphone API.");
    }
    return media.getUserMedia(constraints);
  },
  createContext: () => {
    const Ctor =
      globalThis.AudioContext ??
      (globalThis as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!Ctor) throw new MicError("unsupported", "This browser has no Web Audio support.");
    return new Ctor();
  },
  workletPath: PCM16_WORKLET_PATH,
};

/**
 * Capture constraints. The three processing flags are ON: this is dictation
 * into a text field over a laptop microphone in a room, not a recording, and
 * the transcription model is better served by suppressed keyboard noise than
 * by fidelity. `channelCount: 1` asks the browser to downmix rather than
 * leaving us to; the worklet reads channel 0 either way.
 */
const CONSTRAINTS: MediaStreamConstraints = {
  audio: {
    channelCount: 1,
    echoCancellation: true,
    noiseSuppression: true,
    autoGainControl: true,
  },
  video: false,
};

/**
 * Meter smoothing. Attack fast so speech registers on the syllable that starts
 * it; release slow so the ring settles between words instead of flickering
 * once per phoneme. A meter that tracks the signal exactly is unreadable.
 */
const LEVEL_ATTACK = 0.55;
const LEVEL_RELEASE = 0.12;

export async function openMicrophone(ports: MicPorts = browserMicPorts): Promise<MicCapture> {
  let media: MediaStream;
  try {
    media = await ports.getUserMedia(CONSTRAINTS);
  } catch (err) {
    throw asMicError(err);
  }

  let context: AudioContext;
  let node: AudioWorkletNode;
  try {
    context = ports.createContext();
    // Safari opens contexts suspended until a gesture; the mic press IS one,
    // but resume() must still be called explicitly or no quantum is rendered
    // and the capture is silent with nothing thrown.
    if (context.state === "suspended") await context.resume();
    if (!context.audioWorklet) {
      throw new MicError("unsupported", "This browser has no AudioWorklet support.");
    }
    await context.audioWorklet.addModule(ports.workletPath);
    node = new AudioWorkletNode(context, PCM16_PROCESSOR_NAME, {
      numberOfInputs: 1,
      numberOfOutputs: 0,
    });
  } catch (err) {
    stopTracks(media);
    throw asMicError(err);
  }

  const source = context.createMediaStreamSource(media);
  source.connect(node);

  const downsampler = new Pcm16Downsampler(context.sampleRate);
  let level = 0;
  let stopped = false;
  let controller: ReadableStreamDefaultController<Uint8Array> | null = null;

  const audio = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
    cancel() {
      void teardown();
    },
  });

  node.port.onmessage = (event: MessageEvent) => {
    if (stopped) return;
    const frames = event.data as Float32Array;
    if (!(frames instanceof Float32Array)) return;
    const block = downsampler.encode(frames);
    if (!block) return;
    const weight = block.level > level ? LEVEL_ATTACK : LEVEL_RELEASE;
    level += (block.level - level) * weight;
    try {
      controller?.enqueue(block.bytes);
    } catch {
      // The consumer closed the stream first. Nothing to recover; the next
      // stop() tears the graph down.
    }
  };

  async function teardown(): Promise<void> {
    if (stopped) return;
    stopped = true;
    level = 0;
    try {
      node.port.postMessage("stop");
    } catch {
      /* the node may already be gone */
    }
    node.port.onmessage = null;
    try {
      source.disconnect();
      node.disconnect();
    } catch {
      /* disconnecting twice is not an error worth surfacing */
    }
    stopTracks(media);
    try {
      controller?.close();
    } catch {
      /* already closed by the consumer */
    }
    // Closing the context releases the OS device. Without it the browser's
    // recording indicator stays lit after the person let go, which reads as
    // the page still listening -- and on that point a page owes certainty.
    try {
      await context.close();
    } catch {
      /* a context closed twice is fine */
    }
  }

  return {
    audio,
    sampleRate: TARGET_SAMPLE_RATE,
    level: () => level,
    stop: teardown,
  };
}

function stopTracks(media: MediaStream): void {
  for (const track of media.getTracks()) {
    try {
      track.stop();
    } catch {
      /* a stopped track is what we wanted */
    }
  }
}

/**
 * DOMException names -> our reasons. Anything unrecognised is `unsupported`
 * rather than `denied`: telling someone they refused the microphone when the
 * real fault was a driver is a lie that sends them to the wrong setting.
 */
export function asMicError(err: unknown): MicError {
  if (err instanceof MicError) return err;
  const name = (err as { name?: string } | null)?.name ?? "";
  switch (name) {
    case "NotAllowedError":
    case "SecurityError":
      return new MicError("denied", "The browser is not letting this site use the microphone.");
    case "NotFoundError":
    case "OverconstrainedError":
      return new MicError("no-device", "No microphone is connected.");
    case "NotReadableError":
    case "AbortError":
      return new MicError("device-busy", "Another app is holding the microphone.");
    default:
      return new MicError(
        "unsupported",
        err instanceof Error && err.message ? err.message : "The microphone could not be opened.",
      );
  }
}

/** Frames per posted block, re-exported so the worklet's constant has one home. */
export { CAPTURE_BLOCK_FRAMES };
