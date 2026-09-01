import { describe, expect, it, vi } from "vitest";

import {
  LATCH_BELOW_MS,
  VoiceSession,
  type VoiceCapture,
  type VoicePorts,
  type VoiceState,
  type VoiceTranscriber,
} from "../../src/ask/voiceSession";
import { MicError } from "../../src/ask/micCapture";

// The voice state machine, with no audio stack and no React. Every gesture and
// every refusal below is a thing a person does or a thing a cluster answers;
// the surface only renders what this decides.

class FakeCapture implements VoiceCapture {
  readonly audio = new ReadableStream<Uint8Array>({ start() {} });
  readonly sampleRate = 16000;
  stopped = 0;
  level() {
    return 0.4;
  }
  async stop() {
    this.stopped += 1;
  }
}

class FakeTranscriber implements VoiceTranscriber {
  partial: ((text: string) => void) | null = null;
  private settle: ((text: string) => void) | null = null;
  private fail: ((err: unknown) => void) | null = null;
  calls = 0;

  run(
    _audio: ReadableStream<Uint8Array>,
    opts: { sampleRate: number; onPartial: (text: string) => void; signal: AbortSignal },
  ): Promise<string> {
    this.calls += 1;
    this.partial = opts.onPartial;
    return new Promise<string>((resolve, reject) => {
      this.settle = resolve;
      this.fail = reject;
    });
  }

  complete(text: string) {
    this.settle?.(text);
  }
  refuse(err: unknown) {
    this.fail?.(err);
  }
}

interface Harness {
  session: VoiceSession;
  capture: FakeCapture;
  wire: FakeTranscriber;
  states: VoiceState[];
  transcripts: string[];
  utterances: string[];
  clock: { advance(ms: number): void };
}

function harness(overrides: Partial<VoicePorts> = {}): Harness {
  const capture = new FakeCapture();
  const wire = new FakeTranscriber();
  const states: VoiceState[] = [];
  const transcripts: string[] = [];
  const utterances: string[] = [];
  let now = 1000;

  const session = new VoiceSession(
    {
      openMicrophone: async () => capture,
      transcriber: wire,
      now: () => now,
      ...overrides,
    },
    {
      onState: (s) => states.push(s),
      onTranscript: (t) => transcripts.push(t),
      onUtterance: (t) => utterances.push(t),
    },
  );

  return {
    session,
    capture,
    wire,
    states,
    transcripts,
    utterances,
    clock: {
      advance: (ms) => {
        now += ms;
      },
    },
  };
}

/** Let the microphone promise resolve. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("VoiceSession -- the gesture", () => {
  it("a held press records and sends on release", async () => {
    const h = harness();
    h.session.press();
    await settle();
    expect(h.session.current().phase).toBe("listening");

    h.wire.partial?.("open the");
    h.wire.partial?.("open the fleet");
    // Deltas carry the WHOLE transcript, so the field is REPLACED each time.
    // Appending would render "open theopen the fleet".
    expect(h.transcripts).toEqual(["open the", "open the fleet"]);

    h.clock.advance(LATCH_BELOW_MS + 200);
    h.session.release();
    expect(h.session.current().phase).toBe("transcribing");
    await settle();
    expect(h.capture.stopped).toBe(1);

    h.wire.complete("open the fleet");
    await settle();
    expect(h.utterances).toEqual(["open the fleet"]);
    expect(h.session.current().phase).toBe("idle");
  });

  it("a tap latches, and tapping again ends it", async () => {
    const h = harness();
    h.session.press();
    await settle();

    h.clock.advance(LATCH_BELOW_MS - 100);
    h.session.release();
    expect(h.session.current()).toMatchObject({ phase: "listening", latched: true });
    expect(h.capture.stopped).toBe(0);

    h.session.press();
    await settle();
    expect(h.session.current().phase).toBe("transcribing");
    expect(h.capture.stopped).toBe(1);
  });

  it("a release that beats the permission prompt is still the gesture it was", async () => {
    // The FIRST press blocks on a browser dialog for as long as the person
    // takes to read it. A tap resolves while the session is still `starting`,
    // and dropping it would bring the mic up latched when they expected it to
    // have ended -- or the reverse.
    let allow: (capture: VoiceCapture) => void = () => {};
    const capture = new FakeCapture();
    const h = harness({
      openMicrophone: () =>
        new Promise<VoiceCapture>((resolve) => {
          allow = resolve;
        }),
    });

    h.session.press();
    expect(h.session.current().phase).toBe("starting");
    h.clock.advance(LATCH_BELOW_MS + 500);
    h.session.release();
    expect(h.session.current().phase).toBe("starting");

    allow(capture);
    await settle();
    // Held long -> the utterance ends the moment capture is live.
    expect(h.session.current().phase).toBe("transcribing");
  });

  it("says nothing when nothing was said", async () => {
    const h = harness();
    h.session.press();
    await settle();
    h.clock.advance(LATCH_BELOW_MS + 200);
    h.session.release();
    h.wire.complete("   ");
    await settle();
    expect(h.utterances).toEqual([]);
    expect(h.session.current().phase).toBe("idle");
  });

  it("cancel closes the microphone and sends nothing", async () => {
    const h = harness();
    h.session.press();
    await settle();
    h.wire.partial?.("never mind");
    h.session.cancel();
    await settle();
    expect(h.capture.stopped).toBe(1);
    expect(h.utterances).toEqual([]);
    expect(h.session.current().phase).toBe("idle");
  });

  it("cancelling during the prompt still closes the device it is handed", async () => {
    // Otherwise the browser's recording indicator stays lit for a session
    // nobody wants, which is the one thing a page owes certainty about.
    let allow: (capture: VoiceCapture) => void = () => {};
    const capture = new FakeCapture();
    const h = harness({
      openMicrophone: () =>
        new Promise<VoiceCapture>((resolve) => {
          allow = resolve;
        }),
    });
    h.session.press();
    h.session.cancel();
    allow(capture);
    await settle();
    expect(capture.stopped).toBe(1);
    expect(h.wire.calls).toBe(0);
  });
});

describe("VoiceSession -- refusals", () => {
  it("a blocked microphone leaves a standing explanation, not a phase", async () => {
    // A refusal outlives the attempt that found it: the control keeps
    // explaining itself while the person carries on typing.
    const h = harness({
      openMicrophone: () =>
        Promise.reject(new MicError("denied", "The browser is not letting this site use the microphone.")),
    });
    h.session.press();
    await settle();
    expect(h.session.current()).toMatchObject({
      phase: "idle",
      problem: { kind: "denied" },
    });

    h.session.clearProblem();
    expect(h.session.current().problem).toBeNull();
  });

  it("a cluster with no voice node answers in its own words", async () => {
    const h = harness();
    h.session.press();
    await settle();
    h.wire.refuse(new Error("pushToTalk: streaming transcription is not configured"));
    await settle();

    const problem = h.session.current().problem;
    expect(problem?.kind).toBe("failed");
    // The SDK's own prefix is stripped; the server's sentence is not, because
    // it names the fix and a friendlier paraphrase would drop it.
    expect(problem?.message).toBe("streaming transcription is not configured");
    expect(h.capture.stopped).toBe(1);
  });

  it("does not open a second microphone while one is live", async () => {
    const h = harness();
    const open = vi.fn(async () => h.capture);
    const second = new VoiceSession(
      { openMicrophone: open, transcriber: h.wire, now: () => 0 },
      { onState: () => {}, onTranscript: () => {}, onUtterance: () => {} },
    );
    second.press();
    second.press();
    await settle();
    expect(open).toHaveBeenCalledTimes(1);
  });
});
