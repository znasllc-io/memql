// Can this browser draw the map, and what happens when it cannot.
//
// The scene is WebGL and nothing else. The edge's CSP allows WebGL and
// same-origin workers and BLOCKS WASM (`wasm-unsafe-eval` is absent from the
// one policy every hosted site gets -- component/edge/csp.go), so a library
// that needs a WASM runtime is not an option here and none is used.
//
// WebGL can still be unavailable: a browser with hardware acceleration off, a
// locked-down enterprise profile, a headless environment, a machine whose GPU
// process has crashed. And jsdom, which is what the portal's own tests run
// in.
//
// THAT LAST ONE IS NOT AN ACCIDENT OF TESTING. A page whose only content is a
// canvas is a page that is blank for anyone in the list above, and the
// honest answer is not a stub that makes the tests pass -- it is a fallback
// the map genuinely has. Replay's event list is already a complete linear
// index of every node and every moment (design 4.4: "accessibility is
// structural here, not a second UI"), so the fallback is not a consolation
// screen. It is the same information without the picture.

// probeWebGL asks the browser for a context and throws nothing.
//
// A fresh canvas each time rather than a cached answer: the GPU process can
// die mid-session, and a cached `true` from page load would leave the map
// rendering into a context that no longer exists. The probe is cheap and the
// caller does it once per mount.
export function probeWebGL(): boolean {
  if (typeof document === "undefined") return false;
  // Asked BEFORE touching a canvas, and not only as a fast path: an
  // environment with no WebGL at all (jsdom, and any embedded browser built
  // without it) implements getContext as a stub that logs a loud
  // "not implemented" through its virtual console every time it is called.
  // The answer is the same either way; this is the one that does not fill a
  // test run's output with a failure that is not one.
  if (typeof WebGLRenderingContext === "undefined") return false;
  try {
    const canvas = document.createElement("canvas");
    // webgl2 first because that is what three.js asks for; webgl as the
    // fallback, because a browser that has only WebGL 1 still renders this
    // scene (no feature here needs 2).
    const context =
      canvas.getContext("webgl2") ??
      canvas.getContext("webgl") ??
      canvas.getContext("experimental-webgl");
    return context !== null;
  } catch {
    // getContext is specified not to throw, and implementations disagree.
    // A browser that throws here does not have WebGL, whatever the reason.
    return false;
  }
}

// prefersReducedMotion reads the media query the scene obeys.
//
// Design D7 -- REDUCED MOTION IS THE SCENE'S JOB. The portal's CSS rule
// (src/styles/index.css) cannot reach inside a canvas: there is no element
// per node for a stylesheet to disable an animation on. So the render loop
// reads the preference itself and drops the particles, the drift and the
// overshoot, keeping fades only.
//
// Read as a function rather than a hook here so the pure motion helpers can
// call it too; the canvas subscribes to changes through useReducedMotion.
// Re-exported from the kit since memql#4651: the Constellation needs the same
// read, so there is one implementation of it and this module keeps the name
// the scene's helpers already import.
export { prefersReducedMotion } from "../../ui/motion";
