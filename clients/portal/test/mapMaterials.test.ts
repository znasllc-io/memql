// The map's surface values (epic memql#4661, task memql#4673).
//
// These are numbers a person tunes by LOOKING -- "does this read as a solid or
// as a placeholder" is not a question a test can answer, and the final pass
// happens against the running app in the visual QA sweep (task memql#4675).
//
// What a test CAN hold is the reasoning that makes each of them a decision
// rather than a default, because that is what a later change would undo
// without noticing. Every assertion here has a failure mode written beside it,
// and each one produces a scene that renders perfectly and looks wrong.

import { describe, expect, it } from "vitest";

import {
  BEVEL_RATIO,
  BEVEL_SEGMENTS,
  CONTACT,
  LIGHTING,
  SOLID_MATERIAL,
  dprFor,
} from "../src/nexus/map/materials";

describe("the surface", () => {
  it("is rough enough not to be a mirror", () => {
    // A low-roughness surface with one key light and no environment map is a
    // black object with a white dot on it -- which looks MORE like a
    // placeholder than the flat facets this replaced.
    expect(SOLID_MATERIAL.roughness).toBeGreaterThan(0.25);
    expect(SOLID_MATERIAL.roughness).toBeLessThan(0.7);
  });

  it("is barely metallic, for the mirror-image reason", () => {
    // A metal with nothing to reflect renders nearly black.
    expect(SOLID_MATERIAL.metalness).toBeLessThan(0.3);
  });

  it("keeps a node legible when the key light is behind it", () => {
    // The scene is read as INFORMATION before it is read as a picture. A node
    // that vanishes when the camera swings has stopped carrying its
    // information -- but a high value washes the colour out and every node
    // becomes the same brightness.
    expect(SOLID_MATERIAL.emissiveIntensity).toBeGreaterThan(0);
    expect(SOLID_MATERIAL.emissiveIntensity).toBeLessThan(0.4);
  });
});

describe("the lighting rig", () => {
  it("has a rim light, which is the thing that was missing", () => {
    // From behind and below, it draws a bright edge along every silhouette.
    // That edge is most of what separates "a solid" from "a stand-in", and it
    // is what makes the new bevels visible from angles the key never reaches.
    expect(LIGHTING.rimIntensity).toBeGreaterThan(0);
    expect(LIGHTING.rimPosition[2]).toBeLessThan(0);
  });

  it("keeps the key brighter than the rim", () => {
    // A rim brighter than the key is a scene lit from behind: silhouettes with
    // no modelling, which is the exact look being fixed.
    expect(LIGHTING.keyIntensity).toBeGreaterThan(LIGHTING.rimIntensity);
  });

  it("keeps the ambient low enough to leave form", () => {
    // A high ambient flattens everything into silhouettes.
    expect(LIGHTING.ambientIntensity).toBeLessThan(LIGHTING.keyIntensity);
  });

  it("puts the key above the scene", () => {
    // Where every viewer's intuition puts a light source. Below-and-in-front
    // is theatrical lighting and reads as uncanny.
    expect(LIGHTING.keyPosition[1]).toBeGreaterThan(0);
  });
});

describe("the bevel", () => {
  it("is a FRACTION, so it works on a 0.42 specialist and a 1.5 cluster alike", () => {
    // An absolute bevel would be invisible on one and would consume the other.
    expect(BEVEL_RATIO).toBeGreaterThan(0);
    expect(BEVEL_RATIO).toBeLessThan(0.3);
  });

  it("is subdivided enough to read as a round edge rather than a chamfer", () => {
    // One segment is a single flat cut. Two is where it starts reading as
    // rounded; more is invisible at these sizes and multiplies the vertex
    // count of a mesh that may draw three hundred of them.
    expect(BEVEL_SEGMENTS).toBeGreaterThanOrEqual(2);
    expect(BEVEL_SEGMENTS).toBeLessThanOrEqual(3);
  });
});

describe("the contact", () => {
  it("is wider than the solid, the way a soft shadow spreads", () => {
    expect(CONTACT.radiusRatio).toBeGreaterThan(1);
  });

  it("is subtle enough to read as contact rather than as a second object", () => {
    expect(CONTACT.opacity).toBeGreaterThan(0);
    expect(CONTACT.opacity).toBeLessThan(0.5);
  });

  it("sits just under the node, not on the distant floor", () => {
    // The scene's ground plane is eleven units down. A mark there reads as a
    // separate object; a mark just under the solid reads as contact, which is
    // the whole illusion.
    expect(CONTACT.drop).toBeLessThan(2);
  });
});

describe("the pixel ratio", () => {
  it("is capped, and the cap is the decision", () => {
    // On a 3x phone screen an uncapped canvas renders nine times the pixels of
    // a 1x one, for a picture nobody can tell apart -- the third device pixel
    // is invisible at any real viewing distance and costs four times the
    // fragments of the first.
    const [min, max] = dprFor();
    expect(min).toBe(1);
    expect(max).toBeLessThanOrEqual(2);
    expect(max).toBeGreaterThan(1);
  });
});
