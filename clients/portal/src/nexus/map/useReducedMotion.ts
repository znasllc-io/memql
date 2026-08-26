// The reduced-motion preference, and changes to it.
//
// Design D7: the scene reads the preference itself, because the portal's CSS
// rule cannot reach inside a canvas. Subscribing rather than sampling once
// matters more here than it usually does -- a person who turns the preference
// ON is often doing it BECAUSE of what is currently moving on their screen,
// and a scene that keeps animating until the next navigation has ignored them
// at the exact moment they asked.
//
// The IMPLEMENTATION moved to ui/motion.ts in memql#4651, when the
// Constellation became a second consumer of the identical hook. This module
// stays as the name the map's own files import, so the scene keeps reading as
// a self-contained surface.
export { useReducedMotion } from "../../ui/motion";
