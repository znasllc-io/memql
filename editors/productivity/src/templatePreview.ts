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
