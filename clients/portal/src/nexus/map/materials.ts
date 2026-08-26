// The scene's SURFACE: how its solids catch light (epic memql#4661, task
// memql#4673).
//
// ===========================================================================
// WHY THESE ARE NUMBERS IN A MODULE AND NOT LITERALS IN THE CANVAS
// ===========================================================================
// Two reasons, and the second is the one that matters.
//
// The canvas imports three.js and is therefore the one module in the tree a
// test cannot load (nexusMap.test.tsx enforces that, and jsdom has no WebGL
// anyway). Anything expressed as a literal inside it is a value nothing can
// assert and nothing can find.
//
// And the final tuning of these happens against the RUNNING APP -- the visual
// QA sweep, task memql#4675 -- because "does this read as a solid or as a
// placeholder" is a judgement a person makes by looking. A tuning pass that
// had to edit numbers scattered through a 700-line canvas would be a tuning
// pass nobody could review; here it is a diff of this file.
//
// ===========================================================================
// WHAT THE OWNER ASKED FOR
// ===========================================================================
// "The 3D models don't look very good." They were flat-shaded platonic solids
// at subdivision zero and literal cubes for tasks -- which is what a
// placeholder looks like, because that is what they were. What makes a solid
// read as a solid rather than as a stand-in is: an edge that catches light
// (bevels), a surface that varies across it (roughness under a key light), a
// silhouette that separates from the ground (a rim light from behind), and
// contact where it meets the floor.
//
// Nothing here is a colour. Every colour in the scene comes from the brand
// tokens through palette.ts -- one identity, two renderers -- and a hex in
// this file would be the second copy that drifts.

// The standard-material parameters every solid in the scene shares.
//
// ROUGHNESS IS HIGH ON PURPOSE. A low-roughness surface is a mirror, and a
// mirror in a scene with one key light and no environment map is a black
// object with a white dot on it. 0.38 keeps a broad, soft highlight that
// follows the bevel.
//
// METALNESS IS LOW for the mirror-image reason: a metal with no environment to
// reflect renders nearly black. The small amount that is here is what gives
// the highlight a slight tint of the surface's own colour rather than pure
// white.
export const SOLID_MATERIAL = {
  roughness: 0.38,
  metalness: 0.12,
  // A trace of emissive so a node in shadow is still legible against the
  // ground. The scene is read as information before it is read as a picture,
  // and a node that vanishes when the camera swings behind the key light has
  // stopped carrying its information.
  emissiveIntensity: 0.18,
} as const;

// The lighting rig. Three lights, each doing one job.
export const LIGHTING = {
  // Fill: stops the unlit side going to pure black. Low, because a high
  // ambient flattens everything into silhouettes -- which is exactly the
  // "placeholder" look being fixed.
  ambientIntensity: 0.42,
  // Key: the light that models the form. From above and to one side, which is
  // where every viewer's intuition puts a light source.
  keyIntensity: 1.15,
  keyPosition: [12, 22, 14] as const,
  // RIM: from BEHIND and BELOW, tinted with the scene's accent. This is the
  // single biggest difference between "a solid" and "a placeholder": it draws
  // a bright edge along the silhouette, which separates the object from the
  // ground and makes the bevel visible from angles the key light does not
  // reach.
  rimIntensity: 0.62,
  rimPosition: [-16, -6, -18] as const,
} as const;

// The bevel, as a fraction of a solid's own size.
//
// A FRACTION rather than an absolute, because the scene's solids range from a
// 0.4-unit specialist to a 1.5-unit cluster, and a fixed bevel would be
// invisible on one and would consume the other.
//
// 0.16 is small enough that the silhouette is still recognisably the shape it
// was -- a task is still a cube, which is the thing a person learned to read
// -- and large enough that the edge catches light at any camera angle.
export const BEVEL_RATIO = 0.16;

// Segments across the bevel's curve. Two is where a bevel stops reading as a
// chamfer (a single flat cut) and starts reading as a rounded edge; more than
// three is invisible at these sizes and multiplies the vertex count of an
// instanced mesh that may draw three hundred of them.
export const BEVEL_SEGMENTS = 2;

// The soft contact under each node.
//
// NOT A SHADOW MAP. Real shadows would need a shadow camera, a depth pass and
// a per-frame render target -- on a canvas whose whole design is that it does
// NOTHING when nothing is animating (frameloop="demand"). A single dark,
// blurred disc under each solid buys most of the grounding at the cost of one
// transparent quad, and it costs nothing when the loop is asleep.
export const CONTACT = {
  // Relative to the node's own footprint. Slightly wider than the solid, the
  // way a soft shadow spreads.
  radiusRatio: 1.35,
  opacity: 0.28,
  // How far below the node's centre the disc sits. The scene's ground plane is
  // at -11, but a contact shadow belongs under the OBJECT, not on the floor --
  // a shadow on a distant floor reads as a separate mark rather than as
  // contact.
  drop: 0.55,
} as const;

// dprFor picks the device-pixel-ratio ceiling.
//
// CAPPED AT 2, and the cap is a real decision rather than a default: the third
// device pixel is invisible at any viewing distance a person actually uses,
// and it costs four times the fragments of the first. On a 3x phone screen an
// uncapped canvas renders nine times the pixels of a 1x one for a picture
// nobody can tell apart.
//
// The floor is 1 rather than the device's own value so a low-DPR display still
// gets a full-resolution render -- there is nothing to save there.
export function dprFor(): readonly [number, number] {
  return [1, 2];
}
