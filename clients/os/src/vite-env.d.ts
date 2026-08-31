/// <reference types="vite/client" />

declare module "*?raw" {
  const src: string;
  export default src;
}

// The bundle's own build identifier, substituted by Vite's `define` (see
// vite.config.ts). A global rather than a module export because `define` is a
// compile-time text substitution: there is no module to import it from.
declare const __OS_BUILD__: string;
