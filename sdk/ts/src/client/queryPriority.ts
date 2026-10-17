// Live collections own the read's AbortSignal. Carry its scheduling intent
// without requiring every consumer's reread callback to repeat an SDK option.
// Weak keys expire with the read; no request ids or application data are held.
const backgroundReads = new WeakSet<AbortSignal>();
export function backgroundReadController(): AbortController {
  const controller = new AbortController();
  backgroundReads.add(controller.signal);
  return controller;
}
export function isBackgroundRead(signal?: AbortSignal): boolean {
  return signal !== undefined && backgroundReads.has(signal);
}
