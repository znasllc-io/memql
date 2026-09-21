/**
 * The accessible name of an app's tile in the Launcher or on the phone's home
 * grid, matched whether or not the tile carries the unseen-change marker.
 *
 * The shared marker (src/attention) is an image INSIDE the tile, ahead of the
 * app's name, so a tile carrying one is named "Unseen change Settings". Whether
 * it carries one depends on what the registry declares -- Settings declares
 * Language (memql#5390) -- and not on what a test that opens the app is about,
 * so those tests match the app's name with or without it.
 */
export function appTileName(app: string): RegExp {
  return new RegExp(`^(Unseen change )?${app.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`);
}
