// The scene's colours, read from the brand tokens the rest of the portal uses.
//
// ===========================================================================
// WHY THE SCENE READS CSS RATHER THAN CARRYING ITS OWN HEX
// ===========================================================================
// The portal has ONE visual identity and it lives in brand/ as plain CSS
// custom properties -- imported by the Vite build and by the identity
// service's standalone Tailwind alike, because CSS variables are the one
// format both consume (brand_shared_source_test.go fails the build on a token
// defined anywhere else).
//
// A WebGL canvas cannot be styled by that stylesheet: there is no element per
// node for a rule to reach. So the scene READS the computed values off the
// document root and hands them to three.js as numbers. One identity, two
// renderers, no second copy of the palette to drift -- and the theme toggle
// keeps working, because the tokens it swaps are the ones this reads.
//
// ===========================================================================
// WHICH TOKEN MEANS WHAT
// ===========================================================================
// From the design's section 4.1, and the assignments are semantic rather than
// decorative:
//
//   fg        you, and the goal      -- the two ends of the story
//   accent    agents                 -- the things that act
//   warn      a task that is running -- attention, not alarm
//   danger    a task that failed     -- alarm
//   ok        a task that succeeded
//   data-*    constructs and artifacts -- the same tones the portal uses for
//             values, because that is what they are: things this goal made
//   bg        the ground
//   border    the grid and the road

export interface ScenePalette {
  you: string;
  goal: string;
  agent: string;
  taskQueued: string;
  taskRunning: string;
  taskDone: string;
  taskFailed: string;
  construct: string;
  artifact: string;
  bundle: string;
  ground: string;
  grid: string;
  road: string;
}

// The fallback palette. Used when the tokens cannot be read at all -- a test
// environment with no stylesheet, a canvas mounted before the CSS has landed.
// Deliberately the DARK values rather than a neutral grey: a scene that has
// not resolved its palette should look like the product, not like a debug
// view, and the ground defaults dark because that is what a starfield wants.
const FALLBACK: ScenePalette = {
  you: "#e8eef7",
  goal: "#e8eef7",
  agent: "#5b8cff",
  taskQueued: "#5a6478",
  taskRunning: "#e0a63c",
  taskDone: "#3fb27f",
  taskFailed: "#e2565b",
  construct: "#9a7cf0",
  artifact: "#4fc3d9",
  bundle: "#9a7cf0",
  ground: "#0d1117",
  grid: "#232b36",
  road: "#232b36",
};

// token reads one custom property off an element, trimmed. Returns "" when
// the property is unset or unreadable, which is what makes the per-token
// fallback below a real fallback rather than a blanket one -- a theme that
// defines six of the seven tokens still gets six real colours.
function token(styles: CSSStyleDeclaration | null, name: string): string {
  if (styles === null) return "";
  try {
    return styles.getPropertyValue(name).trim();
  } catch {
    return "";
  }
}

// readPalette resolves the scene palette from the document's current theme.
//
// Called on mount and again when the theme changes, never per frame:
// getComputedStyle forces a style recalculation, and doing that inside a
// render loop is how a scene that draws nothing new still burns a core.
export function readPalette(root: Element | null = typeof document === "undefined" ? null : document.documentElement): ScenePalette {
  let styles: CSSStyleDeclaration | null = null;
  try {
    styles = root === null || typeof getComputedStyle !== "function" ? null : getComputedStyle(root);
  } catch {
    styles = null;
  }

  const pick = (name: string, fallback: string): string => token(styles, name) || fallback;

  return {
    you: pick("--memql-fg", FALLBACK.you),
    goal: pick("--memql-fg", FALLBACK.goal),
    agent: pick("--memql-accent", FALLBACK.agent),
    taskQueued: pick("--memql-fg-subtle", FALLBACK.taskQueued),
    taskRunning: pick("--memql-warn", FALLBACK.taskRunning),
    taskDone: pick("--memql-ok", FALLBACK.taskDone),
    taskFailed: pick("--memql-danger", FALLBACK.taskFailed),
    construct: pick("--memql-data-string", FALLBACK.construct),
    artifact: pick("--memql-data-number", FALLBACK.artifact),
    bundle: pick("--memql-data-string", FALLBACK.bundle),
    ground: pick("--memql-bg", FALLBACK.ground),
    grid: pick("--memql-border", FALLBACK.grid),
    road: pick("--memql-border", FALLBACK.road),
  };
}

// colourForTask maps a derived status to its tone. Kept here rather than in
// the glyph component because Replay's event list uses the same mapping for
// its status dots, and two mappings would let the list and the map disagree
// about what "running" looks like.
export function colourForTask(status: string, palette: ScenePalette): string {
  switch (status) {
    case "running":
      return palette.taskRunning;
    case "succeeded":
      return palette.taskDone;
    case "failed":
      return palette.taskFailed;
    case "cancelled":
    case "paused":
    case "queued":
    default:
      return palette.taskQueued;
  }
}

// toneForTask is the same mapping expressed in the portal's own StatusTone
// vocabulary, for the DOM half of the surface (the event list, the receipt).
// One switch, two vocabularies, so the canvas and the list cannot drift.
export function toneForTask(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "running":
      return "warn";
    case "succeeded":
      return "ok";
    case "failed":
      return "danger";
    default:
      return "neutral";
  }
}
