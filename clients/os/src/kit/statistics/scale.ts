// The two axes of a statistics chart, as arithmetic. Pure, so the numbers a
// chart is drawn from are tested without drawing it.

/**
 * The top of a value axis: the smallest 1, 2, 2.5 or 5 times a power of ten
 * at or above `max`. Nothing above zero (no measured value, or only zeroes)
 * gives 1, so an empty chart still has a scale to draw its grid on.
 */
export function niceMax(max: number): number {
  if (!(max > 0) || !Number.isFinite(max)) return 1;
  let exponent = Math.floor(Math.log10(max));
  // log10 is not exact near a power of ten; correct it by one either way.
  if (scaled(1, exponent) > max) exponent -= 1;
  if (scaled(1, exponent + 1) <= max) exponent += 1;
  for (const step of [1, 2, 2.5, 5]) {
    const top = scaled(step, exponent);
    if (top >= max) return top;
  }
  return scaled(10, exponent);
}

/**
 * `step` times ten to `exponent`. A negative power DIVIDES by the positive
 * one, because a negative power of ten has no exact binary form: `2 * 10 ** -4`
 * is 0.00019999999999999998 where `2 / 10 ** 4` is 0.0002, and an axis top
 * should be the number it reads as.
 */
function scaled(step: number, exponent: number): number {
  return exponent >= 0 ? step * 10 ** exponent : step / 10 ** -exponent;
}

/**
 * The value axis's labelled steps: zero, half the top, and the top.
 *
 * For a series of whole numbers -- visitors, sends -- the middle step is left
 * out when it is not a whole number itself: an axis of 5 reads "0, 5", never
 * "0, 2.5, 5", because nobody was half a visitor. A rate or an average passes
 * `wholeNumbers` false and keeps its middle step.
 */
export function valueTicks(max: number, wholeNumbers = false): number[] {
  const top = niceMax(max);
  const half = top / 2;
  return wholeNumbers && !Number.isInteger(half) ? [0, top] : [0, half, top];
}

/**
 * Which buckets of a `count`-bucket series the time axis labels, given room
 * for `maxLabels` labels.
 *
 * AT A REGULAR INTERVAL, AND ALWAYS THE FIRST AND THE LAST. A whole number of
 * buckets apart ("every fifth day"), because a time axis whose labels fall at
 * 1, 2 and then 4 days apart reads as uneven time. The last bucket is the one
 * the reader most wants named, so it is always labelled, and the label before
 * it gives way when the two would be closer than one label's slot (the axis
 * over its room): `maxLabels` is how many slots fit, so a slot is
 * `(count - 1) / maxLabels` buckets wide.
 *
 * Fewer than two slots still labels the first and the last; a single bucket
 * is labelled once, and an empty series not at all.
 */
export function timeTickIndexes(count: number, maxLabels: number): number[] {
  const buckets = Math.floor(count);
  if (!(buckets > 0)) return [];
  if (buckets === 1) return [0];
  const room = Math.max(2, Number.isFinite(maxLabels) ? Math.floor(maxLabels) : 2);
  if (room >= buckets) return Array.from({ length: buckets }, (_, index) => index);

  const last = buckets - 1;
  const step = Math.ceil(last / (room - 1));
  const ticks: number[] = [];
  for (let index = 0; index < last; index += step) ticks.push(index);
  const before = ticks.at(-1);
  if (ticks.length > 1 && before !== undefined && (last - before) * room < last) ticks.pop();
  ticks.push(last);
  return ticks;
}
