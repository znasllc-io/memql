import { safeEmailHTML } from "./templatePreview.js";
declare function acquireVsCodeApi(): { postMessage(message: unknown): void };
const api = acquireVsCodeApi();
const subject = document.getElementById("subject")!;
const status = document.getElementById("status")!;
const frame = document.getElementById("email") as HTMLIFrameElement;
const text = document.getElementById("text")!;
let version = 0;
for (const mode of ["source", "split", "examples"]) document.getElementById(mode)!.addEventListener("click", () => api.postMessage({ type: mode }));
window.addEventListener("message", event => {
  const message = event.data;
  if (message?.type === "error") { status.textContent = message.message; return; }
  if (message?.type !== "document") return;
  version = message.version;
  subject.textContent = message.template.subject;
  text.textContent = message.template.textBody;
  status.textContent = "Preview — remote images and links are disabled.";
  frame.srcdoc = safeEmailHTML(message.template.htmlBody);
  frame.onload = () => api.postMessage({ type: "rendered", version, subject: message.template.subject });
});
api.postMessage({ type: "ready" });
