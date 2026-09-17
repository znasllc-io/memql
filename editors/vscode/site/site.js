import { examples, coreExamples } from "./example-data.js";

const tabs = [...document.querySelectorAll("[data-example]")];
const code = document.querySelector("#example-code");
const panel = document.querySelector("#code-panel");
const description = document.querySelector("#example-description");
const copy = document.querySelector("#copy-code");
const status = document.querySelector("#copy-status");
const descriptions = [
  "ENGINE · Embed the question and retrieve matching file passages. librarySimilarArtifacts applies the caller’s ownership checks and ranks files by their best matching chunk. EDITOR · Completion and go-to-definition help you inspect the imported builtin and its arguments.",
  "ENGINE · A configured agent turns retrieved passages into a draft. The agent’s model routing and tools remain its configuration. EDITOR · Trace the logic’s inputs, read diagnostics, then inspect a connected execution result.",
  "ENGINE · @cache(300) sets a five-minute TTL on this saved-draft query. Writes to its read concept invalidate cached results. It does not cache agent calls. EDITOR · Run the query with your account and inspect the returned rows.",
  "ENGINE · A new request triggers retrieval, skips the model when there are no sources, and saves a draft under the same ID. EDITOR · Inspect the automation and its event form before an explicit run; saving source alone does not register a trigger.",
];
let selected = 0;
let resetCopy;

// Render source as text nodes; examples never become executable HTML.
function highlight(source, target = code) {
  const fragment = document.createDocumentFragment();
  const tokens =
    /("(?:\\.|[^"\\])*"|\/\/[^\n]*|@[A-Za-z]+|\b(?:concept|shape|trait|spec|query|mutation|tool|logic|automation|builtin|args|filter|sort|paginate|insert|accept|stamp|return|if)\b|\b(?:string|bool|boolean|object|int)\b!?)/g;
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
          : /^(string|bool|boolean|object|int)!?$/.test(token)
            ? "type"
            : "keyword";
    span.textContent = token;
    fragment.append(span);
    end = match.index + token.length;
  }
  fragment.append(document.createTextNode(source.slice(end)));
  target.replaceChildren(fragment);
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
function bindTabs(list, onSelect) {
  list.forEach((tab, index) => {
    tab.addEventListener("click", () => onSelect(index));
    tab.addEventListener("keydown", (event) => {
      let next;
      if (event.key === "ArrowDown" || event.key === "ArrowRight")
        next = (index + 1) % list.length;
      if (event.key === "ArrowUp" || event.key === "ArrowLeft")
        next = (index + list.length - 1) % list.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = list.length - 1;
      if (next !== undefined) {
        event.preventDefault();
        onSelect(next, true);
      }
    });
  });
}
bindTabs(tabs, select);
copy.hidden = false;
copy.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(examples[selected]);
    copy.textContent = "Copied";
    status.textContent =
      "Snippet copied. Download the complete file for all definitions and imports.";
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

const coreTabs = [...document.querySelectorAll("[data-core-example]")];
const coreCode = document.querySelector("#core-code");
const coreDescriptions = [
  "A concept defines typed stored fields and row ownership. A shape projects the fields a reader needs; it does not create another stored record. The researchBriefs query applies this shape below.",
  "A trait is an unbound, reusable row predicate: its fields are checked against each concept where it is applied. A spec binds a predicate to one concept or shape. researchBriefs composes both with the caller’s ownership filter.",
  "A query reads data; a mutation writes a row or a new version. Here accept takes declared caller inputs and stamp supplies the ID, authenticated owner, and initial state. Creating this request starts the automation below.",
];
function selectCore(index, focus = false) {
  coreTabs.forEach((tab, i) => {
    tab.setAttribute("aria-selected", String(i === index));
    tab.tabIndex = i === index ? 0 : -1;
  });
  document
    .querySelector("#core-panel")
    .setAttribute("aria-labelledby", coreTabs[index].id);
  highlight(coreExamples[index], coreCode);
  document.querySelector("#core-description").textContent =
    coreDescriptions[index];
  if (focus) coreTabs[index].focus();
}
bindTabs(coreTabs, selectCore);
selectCore(1);
