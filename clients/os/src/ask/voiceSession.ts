// Ask's voice state machine (epic memql#4747). Pure: no DOM, no React, no
// audio stack -- the microphone and the transcription wire are both seams, so
// every transition below is driven from a test with fixtures. `src/system/`
// holds the shell's state machines for the same reason; this is Ask's.

/** What the surface renders. One of these is true at a time. */
export type VoicePhase = "idle" | "starting" | "listening" | "transcribing";

/** Why voice is not available right now. Held ACROSS phases -- see below. */
export interface VoiceProblem {
  /** Stable key so the surface can decide how loudly to say it. */
  kind: "denied" | "no-device" | "device-busy" | "unsupported" | "failed";
  /** The sentence to render. The server's own words when there are any. */
  message: string;
}

export interface VoiceState {
  phase: VoicePhase;
  /**
   * The standing explanation, or null.
   *
   * Deliberately NOT a phase. A refusal is a fact about this browser that
   * outlives the attempt that discovered it: the mic button must keep
   * explaining itself while the person carries on typing, and typing must not
   * clear it. Modelling it as a phase would force the surface to choose
   * between "idle" and "explaining", and idle is what it actually is.
   */
  problem: VoiceProblem | null;
  /** True while the person is not holding the control but it is still live. */
  latched: boolean;
}

export const IDLE: VoiceState = { phase: "idle", problem: null, latched: false };

/**
 * A press shorter than this LATCHES rather than ends the utterance.
 *
 * One control, two gestures, and the ambiguity is resolved on RELEASE rather
 * than by making the person choose up front: press and you are live either
 * way, which is the property that makes push-to-talk feel instant. 400 ms is
 * comfortably longer than a click and comfortably shorter than the shortest
 * thing anyone says on purpose.
 */
export const LATCH_BELOW_MS = 400;

/** The audio source the session drives. `micCapture.MicCapture` implements it. */
export interface VoiceCapture {
  readonly audio: ReadableStream<Uint8Array>;
  readonly sampleRate: number;
  level(): number;
  stop(): Promise<void>;
}

/** The transcription wire. `sdkVoice.SdkTranscriber` implements it. */
export interface VoiceTranscriber {
  run(
    audio: ReadableStream<Uint8Array>,
    opts: {
      sampleRate: number;
      /** Each delta is the FULL transcript so far, never an increment. */
      onPartial: (text: string) => void;
      signal: AbortSignal;
    },
  ): Promise<string>;
}

export interface VoicePorts {
  openMicrophone(): Promise<VoiceCapture>;
  transcriber: VoiceTranscriber;
  /** Injectable clock so the latch threshold is tested without timing. */
  now?: () => number;
}

export interface VoiceSessionCallbacks {
  onState(state: VoiceState): void;
  /**
   * The live transcript. REPLACES the field, never appends: the wire's deltas
   * are cumulative (AiTranscribeStreamDelta carries the whole transcript so
   * far), and appending them produces "openopen theopen the fleet".
   */
  onTranscript(text: string): void;
  /** A finished utterance. Never fires with blank text. */
  onUtterance(text: string): void;
}

/**
 * One utterance at a time. `press`/`release` are the gesture; `cancel` is Esc
 * and unmount.
 */
export class VoiceSession {
  private state: VoiceState = IDLE;
  private capture: VoiceCapture | null = null;
  private abort: AbortController | null = null;
  private pressedAt = 0;
  /** Set when release beats the permission prompt. See `release()`. */
  private pendingHeldMs: number | null = null;
  private transcript = "";
  private generation = 0;

  constructor(
    private readonly ports: VoicePorts,
    private readonly callbacks: VoiceSessionCallbacks,
  ) {}

  current(): VoiceState {
    return this.state;
  }

  /** Smoothed input level, 0..1. Zero unless a capture is open. */
  level(): number {
    return this.capture?.level() ?? 0;
  }

  press(): void {
    // A press while latched ends the utterance -- the control is the same
    // control, so tapping it again has to be the way out.
    if (this.state.phase === "listening" && this.state.latched) {
      void this.finish();
      return;
    }
    if (this.state.phase !== "idle") return;
    this.pressedAt = this.ports.now?.() ?? Date.now();
    this.pendingHeldMs = null;
    void this.begin();
  }

  release(): void {
    const held = (this.ports.now?.() ?? Date.now()) - this.pressedAt;
    if (this.state.phase === "starting") {
      // THE PERMISSION PROMPT IS SLOWER THAN THE GESTURE. The first ever
      // press blocks on a browser dialog for as long as the person takes to
      // read it, so a tap resolves while `starting` and would otherwise be
      // dropped -- the mic would come up latched with the person expecting it
      // to have already ended, or vice versa. Remember the gesture and apply
      // it the moment capture is live.
      this.pendingHeldMs = held;
      return;
    }
    if (this.state.phase !== "listening" || this.state.latched) return;
    this.applyRelease(held);
  }

  /**
   * End the utterance now, from wherever the gesture came from.
   *
   * Releasing the mic is one way; pressing Send or Enter while the mic is
   * live is another, and it has to mean the same thing. Making Send send the
   * half-transcript instead would leave the microphone open behind a question
   * that had already gone.
   */
  commit(): void {
    if (this.state.phase === "listening") void this.finish();
  }

  /** Abandon: the mic closes and nothing is sent. */
  cancel(): void {
    if (this.state.phase === "idle") return;
    this.generation += 1;
    this.abort?.abort();
    this.abort = null;
    void this.capture?.stop();
    this.capture = null;
    this.transcript = "";
    this.set({ phase: "idle", problem: this.state.problem, latched: false });
  }

  /** Clear a standing explanation -- the person is trying again. */
  clearProblem(): void {
    if (this.state.problem) this.set({ ...this.state, problem: null });
  }

  private applyRelease(heldMs: number): void {
    if (heldMs < LATCH_BELOW_MS) {
      this.set({ ...this.state, latched: true });
      return;
    }
    void this.finish();
  }

  private async begin(): Promise<void> {
    const generation = ++this.generation;
    this.transcript = "";
    this.set({ phase: "starting", problem: null, latched: false });

    let capture: VoiceCapture;
    try {
      capture = await this.ports.openMicrophone();
    } catch (err) {
      if (generation !== this.generation) return;
      this.set({ phase: "idle", problem: problemFor(err), latched: false });
      return;
    }
    if (generation !== this.generation) {
      // Cancelled while the prompt was open. Close what we were handed rather
      // than leaving the recording indicator lit on a session nobody wants.
      void capture.stop();
      return;
    }

    this.capture = capture;
    this.abort = new AbortController();
    this.set({ phase: "listening", problem: null, latched: false });

    const run = this.ports.transcriber
      .run(capture.audio, {
        sampleRate: capture.sampleRate,
        onPartial: (text) => {
          if (generation !== this.generation) return;
          this.transcript = text;
          this.callbacks.onTranscript(text);
        },
        signal: this.abort.signal,
      })
      .then(
        (final) => {
          if (generation !== this.generation) return;
          this.settle(final);
        },
        (err) => {
          if (generation !== this.generation) return;
          this.set({ phase: "idle", problem: problemFor(err), latched: false });
          void this.closeCapture();
        },
      );
    void run;

    const pending = this.pendingHeldMs;
    this.pendingHeldMs = null;
    if (pending !== null) this.applyRelease(pending);
  }

  /** Stop capturing and wait for the transcript the cluster is still writing. */
  private async finish(): Promise<void> {
    if (this.state.phase !== "listening") return;
    this.set({ ...this.state, phase: "transcribing", latched: false });
    // Closing the device ends the audio stream, which is what tells the server
    // the utterance is over. The transcript arrives on the promise begin()
    // already holds.
    await this.closeCapture();
  }

  private async closeCapture(): Promise<void> {
    const capture = this.capture;
    this.capture = null;
    await capture?.stop();
  }

  private settle(final: string): void {
    const text = (final || this.transcript).trim();
    this.abort = null;
    void this.closeCapture();
    this.set({ phase: "idle", problem: null, latched: false });
    // A release with nothing said is not a failure and not an empty question.
    // Nothing happens where nothing was offered.
    if (text) this.callbacks.onUtterance(text);
    this.transcript = "";
  }

  private set(next: VoiceState): void {
    this.state = next;
    this.callbacks.onState(next);
  }
}

/**
 * Any thrown thing -> the sentence the surface renders.
 *
 * A `MicError` carries its own reason. Everything else came off the wire, and
 * the server's message names the fix ("streaming transcription is not
 * configured" is what a cluster answers when no node is serving it) -- so it
 * is passed through rather than replaced with something friendlier and
 * emptier.
 */
export function problemFor(err: unknown): VoiceProblem {
  const reason = (err as { reason?: VoiceProblem["kind"] } | null)?.reason;
  if (reason) return { kind: reason, message: messageOf(err) };
  return { kind: "failed", message: messageOf(err) };
}

function messageOf(err: unknown): string {
  const raw = err instanceof Error ? err.message : "";
  // pushToTalk prefixes its own name; the surface has no use for it.
  const text = raw.replace(/^pushToTalk:\s*/, "").trim();
  return text || "Voice did not work that time.";
}
