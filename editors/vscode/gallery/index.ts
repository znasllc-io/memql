// The gallery's entry point: every scenario module, rendered in every theme.
//
// EVERY FILE UNDER scenarios/ IS IMPORTED HERE. gallery/build.mjs refuses to
// build when one is not, so a scenario cannot be written and then silently
// never rendered.

import { GALLERY_THEMES, wrapDocument, type GalleryTheme, type Scenario } from "./harness.js";
import { scenarios as kit } from "./scenarios/kit.js";
import { scenarios as author } from "./scenarios/author.js";

/** Every scenario, in index order. */
export const SCENARIOS: readonly Scenario[] = [...kit, ...author];

/** One rendered gallery page. */
export interface GalleryPage {
  id: string;
  group: string;
  title: string;
  theme: GalleryTheme;
  /** `<id>.<theme>.html`, relative to dist-gallery/. */
  file: string;
  html: string;
}

/** Render every scenario in every theme, as the editor would show it. */
export function buildGallery(): GalleryPage[] {
  const seen = new Set<string>();
  const pages: GalleryPage[] = [];
  for (const scenario of SCENARIOS) {
    if (!/^[a-z0-9][a-z0-9-]*$/.test(scenario.id)) throw new Error(`gallery: "${scenario.id}" is not a file-safe id`);
    if (seen.has(scenario.id)) throw new Error(`gallery: two scenarios are called "${scenario.id}"`);
    seen.add(scenario.id);
    for (const theme of GALLERY_THEMES) {
      pages.push({
        id: scenario.id,
        group: scenario.group,
        title: scenario.title,
        theme,
        file: `${scenario.id}.${theme}.html`,
        html: wrapDocument(scenario.render(theme), theme),
      });
    }
  }
  return pages;
}
