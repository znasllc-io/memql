import { examples } from "./example-data.js";

const tabs = [...document.querySelectorAll("[data-example]")];
const code = document.querySelector("#example-code");
const panel = document.querySelector("#code-panel");
const description = document.querySelector("#example-description");
const copy = document.querySelector("#copy-code");
const status = document.querySelector("#copy-status");
const descriptions = [
  "Declare the shape of a reading item and its ownership tier. The engine supplies intrinsic fields such as id.",
  "Accept the title from the caller. Stamp the owner from the authenticated actor, and create the item as unfinished. Running this mutation writes a real row.",
  "Filter by the authenticated owner and an optional completion flag. Return the newest 50 rows on the first page.",
  "Expose the same query through a described tool interface. The declaration does not grant an agent access or configure a model provider.",
];
let selected = 0;
let resetCopy;

// Render source as text nodes; examples never become executable HTML.
function highlight(source) {
  const fragment = document.createDocumentFragment();
  const tokens =
    /("(?:\\.|[^"\\])*"|\/\/[^\n]*|@[A-Za-z]+|\b(?:concept|query|mutation|tool|args|filter|sort|paginate|insert|accept|stamp)\b|\b(?:string|bool|boolean)\b!?)/g;
  let end = 0;
  for (const match of source.matchAll(tokens)) {
    fragment.append(document.createTextNode(source.slice(end, match.index)));
    const span = document.createElement("span");
    const token = match[0];
    span.className = token.startsWith("//")
      ? "comment"
      : token.startsWith('"')
        ? "string"
        : token.startsWith("@")
          ? "annotation"
          : /^(string|bool|boolean)!?$/.test(token)
            ? "type"
            : "keyword";
    span.textContent = token;
    fragment.append(span);
    end = match.index + token.length;
  }
  fragment.append(document.createTextNode(source.slice(end)));
  code.replaceChildren(fragment);
}
function select(index, focus = false) {
  selected = index;
  tabs.forEach((tab, i) => {
    tab.setAttribute("aria-selected", String(i === index));
    tab.tabIndex = i === index ? 0 : -1;
  });
  panel.setAttribute("aria-labelledby", tabs[index].id);
  highlight(examples[index]);
  description.textContent = descriptions[index];
  copy.textContent = "Copy snippet";
  status.textContent = "";
  clearTimeout(resetCopy);
  if (focus) tabs[index].focus();
}
tabs.forEach((tab, index) => {
  tab.addEventListener("click", () => select(index));
  tab.addEventListener("keydown", (event) => {
    let next;
    if (event.key === "ArrowDown" || event.key === "ArrowRight")
      next = (index + 1) % tabs.length;
    if (event.key === "ArrowUp" || event.key === "ArrowLeft")
      next = (index + tabs.length - 1) % tabs.length;
    if (event.key === "Home") next = 0;
    if (event.key === "End") next = tabs.length - 1;
    if (next !== undefined) {
      event.preventDefault();
      select(next, true);
    }
  });
});
copy.hidden = false;
copy.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(examples[selected]);
    copy.textContent = "Copied";
    status.textContent =
      "Snippet copied. Download the complete file for all four constructs.";
  } catch {
    copy.textContent = "Select text to copy";
    status.textContent =
      "Clipboard is unavailable. Select the code and copy it, or download the complete file.";
  }
  resetCopy = setTimeout(() => {
    copy.textContent = "Copy snippet";
  }, 4000);
});
select(0);
