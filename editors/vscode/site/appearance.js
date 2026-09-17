// Run before body paint; all assets stay external for the edge's strict CSP.
(() => {
  const key = "memql-editor-site.appearance";
  const choices = new Set(["system", "light", "dark"]);
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const read = () => {
    try {
      const value = localStorage.getItem(key);
      return choices.has(value) ? value : "system";
    } catch {
      return "system";
    }
  };
  let preference = read();
  const apply = () => {
    const theme =
      preference === "system" ? (media.matches ? "dark" : "light") : preference;
    document.documentElement.dataset.theme = theme;
    document.documentElement.dataset.appearance = preference;
    document.querySelectorAll('input[name="appearance"]').forEach((input) => {
      input.checked = input.value === preference;
    });
    // Native browser chrome follows the same resolved choice.
    const mark = document.querySelector(".brand-mark");
    if (mark)
      document
        .querySelector('meta[name="theme-color"]')
        ?.setAttribute("content", getComputedStyle(mark).backgroundColor);
  };
  apply();
  media.addEventListener("change", () => {
    if (preference === "system") apply();
  });
  window.addEventListener("storage", (event) => {
    if (event.key === key || event.key === null) {
      preference = read();
      apply();
    }
  });
  document.addEventListener("DOMContentLoaded", () => {
    document.querySelector(".appearance-controls").hidden = false;
    document.querySelectorAll('input[name="appearance"]').forEach((input) => {
      input.addEventListener("change", () => {
        if (!input.checked || !choices.has(input.value)) return;
        preference = input.value;
        try {
          localStorage.setItem(key, preference);
        } catch {
          /* Selection still works for this visit. */
        }
        apply();
      });
    });
    apply();
  });
})();
