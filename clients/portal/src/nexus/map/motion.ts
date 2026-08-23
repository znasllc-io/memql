// The prototype's timings, and what reduced motion does to them.
//
// ===========================================================================
// THE NUMBERS ARE MEASURED, NOT CHOSEN
// ===========================================================================
// They come from the brainstorm's visual companion (`nexus-layout-v4.html`),
// which is where the arrival animation was tuned live with the owner and then
// watched at replay speed. They are reproduced here rather than re-invented
// because "it felt right at 0.45s" is not something a later reader can
// recover from a scene that reads 0.6.
//
//   720 ms   particles condense into the node
//   560 ms   the node scales in, with a small overshoot
//   380 ms   its edges draw toward nodes that already exist
//   450 ms   one arrival to the next, when a replay is playing at 1x
//
// ===========================================================================
// REDUCED MOTION IS NOT "THE SAME, FASTER"
// ===========================================================================
// Design D7. Cutting the durations would keep every motion and make it
// jerkier, which is the opposite of what the preference asks for. What it
// asks for is that things do not MOVE: no particles travelling, no overshoot
// springing past its target, no ambient drift. What remains is opacity, which
// is not motion.

export interface Timings {
  // Milliseconds a node's particle condense runs for. Zero under reduced
  // motion, which is what switches the particle system off entirely rather
  // than running it invisibly.
  condenseMs: number;
  // The scale-in. Under reduced motion this becomes a pure fade: the node is
  // at full scale from the first frame and only its opacity moves.
  scaleInMs: number;
  // How far past 1 the scale-in springs. Zero under reduced motion.
  overshoot: number;
  // Edge draw.
  edgeDrawMs: number;
  // Ambient breathing amplitude on an agent, in scene units. Zero under
  // reduced motion -- this is the one animation that never stops, so it is
  // the one that matters most to switch off.
  breathAmplitude: number;
  // Seconds between arrivals when a replay plays at 1x.
  arrivalSeconds: number;
}

export const FULL_MOTION: Timings = {
  condenseMs: 720,
  scaleInMs: 560,
  overshoot: 0.18,
  edgeDrawMs: 380,
  breathAmplitude: 0.05,
  arrivalSeconds: 0.45,
};

export const REDUCED_MOTION: Timings = {
  condenseMs: 0,
  scaleInMs: 260,
  overshoot: 0,
  edgeDrawMs: 0,
  breathAmplitude: 0,
  arrivalSeconds: 0.45,
};

export function timingsFor(reduced: boolean): Timings {
  return reduced ? REDUCED_MOTION : FULL_MOTION;
}

// The playback speeds Replay offers (design 6). Exported as data because the
// control renders them and its test asserts over them.
export const PLAYBACK_SPEEDS: readonly number[] = [1, 4, 16];

// easeOutBack is the scale-in curve: it reaches 1 and springs a little past
// before settling, which is the "arrival" the prototype has and a plain
// ease-out does not. `overshoot` of 0 collapses it to a plain ease-out, which
// is exactly what reduced motion wants -- so there is one curve, not two.
export function easeOutBack(t: number, overshoot: number): number {
  const clamped = t <= 0 ? 0 : t >= 1 ? 1 : t;
  if (overshoot === 0) return 1 - Math.pow(1 - clamped, 3);
  // The standard back curve, with `c1` derived from the requested overshoot
  // rather than the usual hard-coded 1.70158, so the constant above is the
  // knob and this is just arithmetic.
  const c1 = overshoot * 10;
  const c3 = c1 + 1;
  return 1 + c3 * Math.pow(clamped - 1, 3) + c1 * Math.pow(clamped - 1, 2);
}
