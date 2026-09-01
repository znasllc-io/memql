// Browser audio -> the bytes the transcription wire actually wants.
//
// ===========================================================================
// WHY THIS FILE EXISTS AT ALL: `format` IS A LABEL THE SERVER DOES NOT READ
// ===========================================================================
// AiTranscribeStreamStart carries `format`, and the obvious browser capture --
// MediaRecorder, which yields webm/opus -- can declare `format: "webm"` and
// look correct. It is not. The cluster's default STT provider is
// openai-realtime, and integrations/stt/openai_realtime.go passes only
// SampleRate through to the ASR client: it never reads Format, and the client
// resamples what it is handed from 16 kHz to the 24 kHz PCM the Realtime
// session is configured for. Hand it opus and it resamples opus's container
// bytes as though they were samples. Nothing errors. The session opens, chunks
// flow, and the transcript comes back as plausible nonsense.
//
// (The other provider, openai-whisper, DOES decode webm -- and is batch, so it
// emits no interim deltas at all. Live transcript and container audio are
// mutually exclusive; live transcript is the feature.)
//
// So the browser owns the conversion: 16 kHz, mono, signed 16-bit
// little-endian PCM, matching core/audio.PolyphonSampleRate. Everything here
// is pure and synchronous so it is tested against fixtures rather than against
// a microphone.

/** What the wire wants. `core/audio.PolyphonSampleRate` on the Go side. */
export const TARGET_SAMPLE_RATE = 16000;

/**
 * Source frames per posted block, in the capture graph's own rate.
 *
 * 2048 frames is 16 render quanta, ~43 ms at the 48 kHz most browsers open a
 * context at, which lands each chunk near the 1280 bytes / 40 ms the Go SDK
 * recommends (sdk/go/voice/pushtotalk.go). Smaller means more envelopes for no
 * transcription benefit; larger is latency the person feels as the transcript
 * lagging their voice.
 */
export const CAPTURE_BLOCK_FRAMES = 2048;

/** A converted block: the wire bytes, plus how loud it was. */
export interface Pcm16Block {
  bytes: Uint8Array;
  /** RMS amplitude of this block, 0..1. Raw -- the meter maps it, not this. */
  level: number;
}

/**
 * Float32 capture blocks at any source rate -> 16 kHz PCM16 bytes.
 *
 * STATEFUL ON PURPOSE. A capture block boundary almost never falls on an
 * output-sample boundary, so the resampler carries the unconsumed tail and a
 * fractional read position between calls. A stateless per-block converter
 * would restart its window grid every block, which at 48 kHz is a seam every
 * 43 ms -- a periodic artefact at ~23 Hz that a speech model hears as texture.
 *
 * Downsampling is a BOX AVERAGE over each output window, not point sampling.
 * Taking every third sample is one line shorter and aliases everything above
 * 8 kHz down into the speech band, where sibilance turns into low burble; the
 * average is a crude anti-alias filter that costs the same additions the copy
 * already makes. It is not a good filter. It is a real one, and the
 * alternative on this budget was none.
 */
export class Pcm16Downsampler {
  private readonly ratio: number;
  private carry: Float32Array = new Float32Array(0);
  /** Fractional read position within `carry` + the incoming block. */
  private cursor = 0;

  constructor(sourceRate: number) {
    if (!(sourceRate > 0)) throw new Error("Pcm16Downsampler: sourceRate must be positive");
    this.ratio = sourceRate / TARGET_SAMPLE_RATE;
  }

  /** Convert one capture block. Returns null when it yields no whole sample. */
  encode(frames: Float32Array): Pcm16Block | null {
    const buf = concat(this.carry, frames);
    const out: number[] = [];
    let sumSquares = 0;

    let pos = this.cursor;
    while (pos + this.ratio <= buf.length) {
      const from = Math.floor(pos);
      const to = Math.min(buf.length, Math.max(from + 1, Math.ceil(pos + this.ratio)));
      let sum = 0;
      for (let i = from; i < to; i += 1) sum += buf[i] ?? 0;
      const sample = sum / (to - from);
      out.push(sample);
      sumSquares += sample * sample;
      pos += this.ratio;
    }

    const consumed = Math.floor(pos);
    this.carry = buf.subarray(consumed);
    this.cursor = pos - consumed;

    if (out.length === 0) return null;
    return {
      bytes: toPcm16LE(out),
      level: Math.sqrt(sumSquares / out.length),
    };
  }
}

/** Clamp to [-1, 1] and write signed 16-bit little-endian. */
function toPcm16LE(samples: number[]): Uint8Array {
  const bytes = new Uint8Array(samples.length * 2);
  const view = new DataView(bytes.buffer);
  for (let i = 0; i < samples.length; i += 1) {
    const clamped = Math.max(-1, Math.min(1, samples[i] ?? 0));
    // Asymmetric on purpose: 16-bit signed runs -32768..32767, so scaling the
    // negative half by 32768 uses the full range without wrapping +1.0 to
    // -32768, which is the click a naive `* 32768` puts on every clipped peak.
    view.setInt16(i * 2, clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff, true);
  }
  return bytes;
}

function concat(head: Float32Array, tail: Float32Array): Float32Array {
  if (head.length === 0) return tail;
  const out = new Float32Array(head.length + tail.length);
  out.set(head, 0);
  out.set(tail, head.length);
  return out;
}
