import { describe, expect, it } from "vitest";

import { Pcm16Downsampler, TARGET_SAMPLE_RATE } from "../../src/ask/pcm16";

// The conversion is the whole reason Ask voice works at all: openai-realtime
// ignores the `format` field and treats every byte as 16 kHz PCM16, so a
// mistake here does not error -- it transcribes noise. These are the fixtures
// that make that failure loud.

function samplesOf(bytes: Uint8Array): number[] {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  return Array.from({ length: bytes.byteLength / 2 }, (_, i) => view.getInt16(i * 2, true));
}

function ramp(n: number, from = 0): Float32Array {
  const out = new Float32Array(n);
  for (let i = 0; i < n; i += 1) out[i] = Math.sin((from + i) * 0.01) * 0.5;
  return out;
}

describe("Pcm16Downsampler", () => {
  it("decimates 48 kHz to the 16 kHz the wire declares", () => {
    const block = new Pcm16Downsampler(48000).encode(ramp(3072));
    expect(block).not.toBeNull();
    expect(block!.bytes.byteLength / 2).toBe(3072 / (48000 / TARGET_SAMPLE_RATE));
  });

  it("carries state across block boundaries instead of restarting the window grid", () => {
    // Three capture blocks must convert to exactly what one long block does.
    // A stateless per-block converter passes every other test here and puts a
    // seam in the audio every 43 ms -- a periodic artefact a speech model
    // hears as texture, and nothing in the pipeline would report it.
    const whole = new Pcm16Downsampler(48000).encode(ramp(6144));

    const streaming = new Pcm16Downsampler(48000);
    const parts: number[] = [];
    for (let i = 0; i < 3; i += 1) {
      const block = streaming.encode(ramp(2048, i * 2048));
      if (block) parts.push(...samplesOf(block.bytes));
    }

    expect(parts.length).toBe(6144 / 3);
    expect(parts).toEqual(samplesOf(whole!.bytes));
  });

  it("does not drift on a non-integer ratio", () => {
    // 44100 -> 16000 is 2.75625:1, so no block boundary lands on an output
    // sample. The count must stay within a sample of the exact ratio over a
    // long utterance: a converter that realigns its window grid per block
    // loses a fraction of a sample each time, and a minute of dictation ends
    // up seconds out of step with its own transcript.
    const blocks = 30;
    const frames = 2048;
    const down = new Pcm16Downsampler(44100);
    let produced = 0;
    for (let i = 0; i < blocks; i += 1) {
      const block = down.encode(ramp(frames, i * frames));
      produced += block ? block.bytes.byteLength / 2 : 0;
    }
    const exact = (blocks * frames) / (44100 / TARGET_SAMPLE_RATE);
    expect(Math.abs(produced - exact)).toBeLessThan(2);
  });

  it("clamps a full-scale peak without wrapping it to the opposite rail", () => {
    // `* 32768` turns +1.0 into -32768, which is a click on every clipped
    // peak -- audible, and exactly the samples a loud speaker produces.
    const hot = new Float32Array(6).fill(1);
    hot[3] = -1;
    hot[4] = 2; // over-range input must clamp, not wrap
    const block = new Pcm16Downsampler(TARGET_SAMPLE_RATE).encode(hot);
    const samples = samplesOf(block!.bytes);
    expect(Math.max(...samples)).toBe(32767);
    expect(Math.min(...samples)).toBe(-32768);
    expect(samples.every((s) => s >= -32768 && s <= 32767)).toBe(true);
  });

  it("reports silence as no level and speech as some", () => {
    const silent = new Pcm16Downsampler(48000).encode(new Float32Array(3072));
    expect(silent!.level).toBe(0);

    const loud = new Pcm16Downsampler(48000).encode(new Float32Array(3072).fill(0.5));
    expect(loud!.level).toBeGreaterThan(0.4);
    expect(loud!.level).toBeLessThanOrEqual(1);
  });

  it("averages the window rather than point-sampling it", () => {
    // Point sampling 48k -> 16k keeps every third sample, so an alternating
    // signal survives at full amplitude and aliases into the speech band. A
    // box average of three samples of [+1,-1,+1,...] lands near +/-1/3.
    const alternating = new Float32Array(3072);
    for (let i = 0; i < alternating.length; i += 1) alternating[i] = i % 2 === 0 ? 1 : -1;
    const samples = samplesOf(new Pcm16Downsampler(48000).encode(alternating)!.bytes);
    expect(Math.max(...samples.map(Math.abs))).toBeLessThan(32767 / 2);
  });

  it("yields nothing rather than a partial sample when a block is too short", () => {
    expect(new Pcm16Downsampler(48000).encode(new Float32Array(2))).toBeNull();
  });
});
