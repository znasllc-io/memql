/*
 * The Ask microphone tap (epic memql#4747).
 *
 * One job: hand the page fixed-size blocks of the raw capture signal. All the
 * arithmetic -- resampling to 16 kHz, PCM16 conversion, the level -- lives in
 * src/ask/pcm16.ts, where it is pure and fixture-tested. A worklet runs on the
 * audio thread with no DOM, no modules and no test harness, so the less of the
 * feature lives here the more of it can be proved.
 *
 * Blocks are POSTED, not shared. `process` is called every 128 frames; sending
 * one message per quantum is ~375 per second, so blocks accumulate to
 * CAPTURE_BLOCK_FRAMES (2048, ~43 ms at 48 kHz) and post once. The buffer is
 * transferred rather than copied, so a fresh one is allocated each time.
 *
 * Plain JS on purpose: this file is copied verbatim into the bundle root (vite
 * `public/`), outside the module graph and the typechecker, the same way
 * download-sw.js is. It is not a bundler convenience -- the edge answers
 * `script-src 'self'`, which refuses a blob: worklet URL, so the module must
 * be a real same-origin file. Keep it small enough to review by reading.
 */

"use strict";

var BLOCK_FRAMES = 2048;

class Pcm16TapProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.block = new Float32Array(BLOCK_FRAMES);
    this.at = 0;
    this.stopped = false;
    var self = this;
    this.port.onmessage = function (event) {
      if (event.data === "stop") self.stopped = true;
    };
  }

  process(inputs) {
    if (this.stopped) return false;
    var input = inputs[0];
    var channel = input && input[0];
    // No channel yet is normal before the track produces audio; staying alive
    // is the difference between a slow microphone and a dead one.
    if (!channel) return true;

    for (var i = 0; i < channel.length; i += 1) {
      this.block[this.at] = channel[i];
      this.at += 1;
      if (this.at === BLOCK_FRAMES) {
        this.port.postMessage(this.block, [this.block.buffer]);
        this.block = new Float32Array(BLOCK_FRAMES);
        this.at = 0;
      }
    }
    return true;
  }
}

registerProcessor("memql-pcm16-tap", Pcm16TapProcessor);
