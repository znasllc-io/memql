/**
 * Tailwind config for the identity web app.
 *
 * Source scanning: every .templ file in component/identity/web/templ
 * AND the templ-generated *_templ.go files (templ writes the class
 * names into Go string literals, so Tailwind needs them too). The
 * generated files are committed alongside the .templ sources so this
 * just-in-time compilation has every class name visible.
 */
const path = require("path");

module.exports = {
  content: [
    // Absolute paths resolved from this config file's directory so the
    // CLI works regardless of which CWD invokes it.
    path.join(__dirname, "..", "templ", "**", "*.templ"),
    path.join(__dirname, "..", "templ", "**", "*_templ.go"),
  ],
  // Class names that are constructed dynamically in templ source
  // (e.g. `"crop-stage--" + c.Variant`) don't appear as literals in
  // the scanned files, so Tailwind would prune the corresponding
  // @layer components rules. Safelist the variant suffixes so the
  // image-upload + crop-stage variant rules survive the build.
  safelist: [
    "crop-stage--logo",
    "crop-stage--icon",
    "image-upload-idle--logo",
    "image-upload-idle--icon",
  ],
  theme: {
    extend: {
      colors: {
        // Brand color is injected at runtime via the --brand-primary
        // CSS custom property the layout's <body style="..."> sets.
        // Reference it in components as `bg-brand` / `text-brand` /
        // `ring-brand`. We use var() (not the rgb() pattern Tailwind
        // recommends for alpha-value support) because the brand color
        // is a single CSS variable, not a triplet — alpha utilities
        // (bg-brand/50 etc.) aren't supported as a result, but plain
        // brand color usage works everywhere.
        brand: {
          DEFAULT: "var(--brand-primary)",
          hover:   "var(--brand-primary-hover)",
        },
      },
      fontFamily: {
        sans: ["system-ui", "-apple-system", "BlinkMacSystemFont", "Segoe UI", "Roboto", "Helvetica Neue", "Arial", "sans-serif"],
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Monaco", "Consolas", "monospace"],
      },
    },
  },
  plugins: [],
};
