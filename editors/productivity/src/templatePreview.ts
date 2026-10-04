import type { EmailTemplate } from "./templates.js";

const tags = /\{\{([^{}\r\n]{1,120}?)\}\}/g;
const supported = (key: string) => ["displayName", "email", "campaignName", "accountName"].includes(key) || key.startsWith("fields.") && key.length > 7;
export function sampleFields(template: EmailTemplate): string[] {
  return [...new Set([template.subject, template.textBody, template.htmlBody].flatMap(body =>
    [...body.matchAll(tags)].map(match => match[1]).filter(supported)))].sort().slice(0, 64);
}

// Sample values stay in this preview. They are never written into the template
// or treated as a recipient record, and replacement is literal and single-pass.
export function sampleTemplate(template: EmailTemplate, values: Record<string, string>): EmailTemplate {
  const escape = (value: string) => value.replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;").replace(/'/g,"&#39;");
  const render = (body: string, html: boolean) => body.replace(tags, (tag, key: string) => {
    if (!supported(key) || !Object.hasOwn(values,key)) return tag;
    return html ? escape(values[key]) : values[key];
  });
  return {subject:render(template.subject,false),textBody:render(template.textBody,false),htmlBody:render(template.htmlBody,true)};
}

// Render in an inert document first, then keep only email presentation elements.
// A reference or generated template cannot make the preview fetch remote assets,
// submit a form, navigate, or execute code. Original source remains editable.
export function safeEmailHTML(html: string): string {
  const doc = new DOMParser().parseFromString(html, "text/html");
  const allowed = new Set("a b blockquote br caption center code col colgroup dd div dl dt em font h1 h2 h3 h4 h5 h6 hr i img li ol p pre s small span strong sub sup table tbody td th thead tfoot tr u ul style".split(" "));
  const attributes = new Set("align alt bgcolor border cellpadding cellspacing class color colspan dir face height lang role rowspan size style title valign width".split(" "));
  for (const node of Array.from(doc.querySelectorAll("*"))) {
    if (["html", "head", "body"].includes(node.localName)) continue;
    if (!allowed.has(node.localName)) { node.remove(); continue; }
    for (const attr of Array.from(node.attributes)) {
      if (attr.name === "src" && node.localName === "img" && /^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/=]+$/.test(attr.value)) continue;
      if (!attributes.has(attr.name)) node.removeAttribute(attr.name);
    }
    if (node.localName === "img" && !node.hasAttribute("src")) {
      const label = doc.createElement("span"); label.textContent = `[Image: ${node.getAttribute("alt") || "external asset"}]`; node.replaceWith(label);
    }
  }
  const styles = Array.from(doc.head.querySelectorAll("style")).map(style => style.outerHTML).join("");
  return `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'">${styles}</head><body>${doc.body.innerHTML}</body></html>`;
}
