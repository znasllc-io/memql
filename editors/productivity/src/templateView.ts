import { safeEmailHTML, sampleFields, sampleTemplate } from "./templatePreview.js";
import type { EmailTemplate } from "./templates.js";
declare function acquireVsCodeApi(): { postMessage(message: unknown): void };
const api = acquireVsCodeApi();
const subject = document.getElementById("subject")!;
const status = document.getElementById("status")!;
const frame = document.getElementById("email") as HTMLIFrameElement;
const text = document.getElementById("text")!;
let version = 0;
let template: EmailTemplate | undefined;
const samples: Record<string,string> = Object.create(null);
const samplePanel = document.getElementById("samples")!;
const renderPreview = () => {
  if (!template) return;
  const preview = sampleTemplate(template, samples);
  subject.textContent = preview.subject;
  text.textContent = preview.textBody;
  frame.srcdoc = safeEmailHTML(preview.htmlBody || `<pre>${preview.textBody.replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;")}</pre>`);
  const renderedVersion = version, renderedSubject = template.subject;
  frame.onload = () => api.postMessage({ type: "rendered", version: renderedVersion, subject: renderedSubject });
};
for (const mode of ["source", "split", "examples", "publish"]) document.getElementById(mode)!.addEventListener("click", () => api.postMessage({ type: mode }));
window.addEventListener("message", event => {
  const message = event.data;
  if (message?.type === "error") { status.textContent = message.message; return; }
  if (message?.type !== "document") return;
  version = message.version;
  template = message.template;
  status.textContent = "Preview — remote images and links are disabled.";
  samplePanel.replaceChildren();
  for (const key of sampleFields(message.template)) {
    const label = document.createElement("label"); label.textContent = `{{${key}}}`;
    const input = document.createElement("input"); input.type = "text"; input.maxLength = 2000; input.placeholder = "Sample value"; input.value = samples[key] || "";
    input.addEventListener("input", () => { samples[key] = input.value; renderPreview(); });
    label.append(input); samplePanel.append(label);
  }
  renderPreview();
});
api.postMessage({ type: "ready" });
