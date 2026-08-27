// The memory field's geometry (spec F): a sparse lattice of time-dots with
// rare links, drifting glacially. Pure and deterministic under a seed --
// the canvas component only paints what this returns, and the tests pin
// the determinism so a theme pack can rely on the same field.

export interface FieldDot {
  x: number;
  y: number;
  r: number;
  /** Drift phase + amplitude; position at time t derives from these. */
  phase: number;
  amp: number;
}

export interface FieldLink {
  from: number;
  to: number;
}

export interface Field {
  dots: FieldDot[];
  links: FieldLink[];
}

export interface FieldOptions {
  /** Lattice cell size in px (one candidate dot per cell). */
  cell: number;
  /** Chance a cell holds a dot. */
  density: number;
  /** Chance a dot links to its nearest later neighbor within reach. */
  linkChance: number;
  /** Max link length in px. */
  linkReach: number;
}

export const DEFAULT_FIELD: FieldOptions = {
  cell: 110,
  density: 0.5,
  linkChance: 0.14,
  linkReach: 260,
};

/** mulberry32 -- tiny deterministic PRNG, plenty for scenography. */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a += 0x6d2b79f5;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function generateField(
  seed: number,
  width: number,
  height: number,
  opts: FieldOptions = DEFAULT_FIELD,
): Field {
  const rand = mulberry32(seed);
  const dots: FieldDot[] = [];
  const cols = Math.max(1, Math.ceil(width / opts.cell));
  const rows = Math.max(1, Math.ceil(height / opts.cell));
  for (let c = 0; c < cols; c += 1) {
    for (let r = 0; r < rows; r += 1) {
      const roll = rand();
      const jx = rand();
      const jy = rand();
      const size = rand();
      const phase = rand();
      const amp = rand();
      if (roll > opts.density) continue;
      dots.push({
        x: (c + 0.15 + jx * 0.7) * opts.cell,
        y: (r + 0.15 + jy * 0.7) * opts.cell,
        r: 0.8 + size * 1.4,
        phase: phase * Math.PI * 2,
        amp: 2 + amp * 4,
      });
    }
  }

  const links: FieldLink[] = [];
  for (let i = 0; i < dots.length; i += 1) {
    if (rand() > opts.linkChance) continue;
    const from = dots[i];
    if (!from) continue;
    let best = -1;
    let bestDist = opts.linkReach;
    for (let j = i + 1; j < dots.length; j += 1) {
      const candidate = dots[j];
      if (!candidate) continue;
      const dist = Math.hypot(candidate.x - from.x, candidate.y - from.y);
      if (dist < bestDist) {
        best = j;
        bestDist = dist;
      }
    }
    if (best >= 0) links.push({ from: i, to: best });
  }
  return { dots, links };
}

/** A dot's position at time t (seconds). Drift is a slow, tiny orbit. */
export function dotAt(dot: FieldDot, t: number): { x: number; y: number } {
  const angle = dot.phase + t * 0.05;
  return { x: dot.x + Math.cos(angle) * dot.amp, y: dot.y + Math.sin(angle * 0.8) * dot.amp };
}
