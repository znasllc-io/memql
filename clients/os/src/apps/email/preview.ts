/** Keep a test email readable without executing it or loading tracking URLs. */
export function emailPreview(html: string): string {
  const doc = new DOMParser().parseFromString(html, "text/html");
  const allowed = new Set(["A", "P", "BR", "DIV", "SPAN", "STRONG", "B", "EM", "I", "U", "H1", "H2", "H3", "H4", "UL", "OL", "LI", "TABLE", "TBODY", "THEAD", "TR", "TD", "TH", "HR", "BLOCKQUOTE", "PRE", "CODE"]);
  for (const element of [...doc.body.querySelectorAll("*")]) {
    if (!allowed.has(element.tagName)) { element.remove(); continue; }
    const href = element.getAttribute("href") ?? "";
    for (const attribute of [...element.attributes]) element.removeAttribute(attribute.name);
    if (element.tagName === "A" && /^https?:\/\//i.test(href.trim())) {
      element.setAttribute("href", href.trim());
      element.setAttribute("target", "_blank");
      element.setAttribute("rel", "noopener noreferrer");
    }
  }
  return '<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'; base-uri \'none\'; form-action \'none\'"><meta name="referrer" content="no-referrer"><style>body{font:15px/1.6 system-ui;padding:20px;overflow-wrap:anywhere}table{max-width:100%}pre{white-space:pre-wrap}a{color:#265bd7}</style>' + doc.body.innerHTML;
}
