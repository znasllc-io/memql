import { describe, expect, it } from "vitest";
import { emailPreview } from "../../src/apps/email/preview";

describe("email preview isolation", () => {
	 it("shows embedded raster assets without allowing remote or executable images", () => {
	   const raster = "data:image/png;base64,iVBORw0KGgo=";
	   const doc = new DOMParser().parseFromString(emailPreview(`<img src="${raster}" alt="Client logo" onerror="alert(1)"><img src="data:image/svg+xml;base64,PHN2Zy8+"><img src="https://tracking.test/image">`), "text/html");
	   expect(doc.querySelectorAll("img")).toHaveLength(1);
	   expect(doc.querySelector("img")?.getAttribute("src")).toBe(raster);
	   expect(doc.querySelector("img")?.getAttribute("alt")).toBe("Client logo");
	   expect(doc.querySelector("[onerror]")).toBeNull();
	 });
  it("removes active content, tracking requests and dangerous links, retaining sign-in links", () => {
    const result = emailPreview(`<base href="https://evil.test"><style>body{background:url(https://track.test)}</style><script>alert(1)</script><p onclick="alert(2)" style="background:url(https://track.test)">Hello</p><img src="https://track.test/pixel"><iframe src="https://evil.test"></iframe><form action="https://evil.test"><input></form><svg onload="alert(3)"></svg><a href="javascript:alert(4)">Unsafe</a><a href="data:text/html,test">Data</a><a href="https://identity.example.test/complete?token=123">Sign in</a>`);
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(doc.querySelectorAll("script,img,iframe,form,input,svg,base,[onclick],[onload],[style]")).toHaveLength(0);
    expect(result).not.toContain("track.test");
    expect(result).not.toContain("evil.test");
    expect(result).not.toContain("javascript:");
    expect(result).not.toContain("data:text");
    expect(doc.querySelector("a[href]")?.getAttribute("href")).toBe("https://identity.example.test/complete?token=123");
    expect(doc.querySelector("a[href]")?.getAttribute("rel")).toBe("noopener noreferrer");
    expect(doc.querySelector("meta[http-equiv]")?.getAttribute("content")).toContain("default-src 'none'");
  });

  // RULING 21: the preview is the email as it is sent, so its own look stays --
  // inline styles, style blocks and the table attributes an email is laid out
  // with -- and only what would LOAD something goes.
  it("keeps the email's inline styles, style blocks and table attributes", () => {
    const html =
      '<html><head><style>@media (max-width:599px){.ecw{width:100%!important}}</style></head>' +
      '<body style="background-color:#eef0f3;margin:0">' +
      '<table width="600" align="center" bgcolor="#ffffff" cellpadding="0" cellspacing="0" border="0" class="ecw" role="presentation">' +
      '<tr><td valign="top" style="color:#2b2b2b;font-size:16px">Hello</td></tr></table></body></html>';
    const result = emailPreview(html);
    const doc = new DOMParser().parseFromString(result, "text/html");

    const cell = doc.querySelector("td");
    expect(cell?.getAttribute("style")).toBe("color:#2b2b2b;font-size:16px");
    expect(cell?.getAttribute("valign")).toBe("top");
    expect(result).toContain("@media (max-width:599px){.ecw{width:100%!important}}");
    const table = doc.querySelector("table");
    for (const [name, value] of [["width", "600"], ["align", "center"], ["bgcolor", "#ffffff"], ["cellpadding", "0"], ["cellspacing", "0"], ["border", "0"]]) {
      expect(table?.getAttribute(name!)).toBe(value);
    }
    // The class the mobile rule names, and the email's own background.
    expect(table?.getAttribute("class")).toBe("ecw");
    expect(doc.body.getAttribute("style")).toBe("background-color:#eef0f3;margin:0");
  });

  it("drops any style that would load something", () => {
    const html =
      '<html><head><style>@import url(https://track.test/a.css);@import "https://track.test/b.css";' +
      'p{color:red;background-image:url(https://track.test/p.png)}' +
      '.x{width:expression(alert(1));height:10px}' +
      '.y{behavior:url(x.htc);-moz-binding:url(https://track.test/b.xml#x);margin:0}' +
      '@font-face{font-family:Brand;src:url(https://track.test/f.woff)}' +
      '.z{background:\\75 rl(https://track.test/escaped.png);padding:2px}</style></head>' +
      '<body><p class="x" style="color:#2b2b2b;background:url(https://track.test/inline.png);font-size:16px">Hi</p>' +
      '<div style="background:URL( https://track.test/upper.png )">There</div></body></html>';
    const result = emailPreview(html);
    const doc = new DOMParser().parseFromString(result, "text/html");

    expect(result).not.toContain("track.test");
    expect(result).not.toMatch(/@import/i);
    expect(result).not.toMatch(/expression\(/i);
    expect(result).not.toMatch(/behavior|binding|x\.htc/i);
    // What did not load anything stays, rule by rule.
    const css = [...doc.querySelectorAll("style")].map((style) => style.textContent).join("");
    expect(css).toContain("p{color:red}");
    expect(css).toContain(".x{height:10px}");
    expect(css).toContain(".y{margin:0}");
    expect(css).toContain(".z{padding:2px}");
    expect(doc.querySelector("p")?.getAttribute("style")).toBe("color:#2b2b2b;font-size:16px");
    // A style left with nothing in it is no style at all.
    expect(doc.querySelector("div")?.hasAttribute("style")).toBe(false);
    expect(doc.querySelector("div")?.textContent).toBe("There");
  });

  it("keeps the words of an element it does not know, and drops what it holds that it does not show", () => {
    const result = emailPreview(
      '<body><center><font color="#2b2b2b" face="Georgia">Kept</font></center><o:p>Office words</o:p>' +
        '<!--[if mso]><table><tr><td>Outlook only</td></tr></table><![endif]-->' +
        '<noscript>Turn on scripts</noscript><template><p>Never shown</p></template><p>End</p></body>',
    );
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(doc.querySelector("font")?.getAttribute("color")).toBe("#2b2b2b");
    expect(doc.querySelector("center")?.textContent).toBe("Kept");
    // An unknown wrapper gives way to its words rather than taking them with it.
    expect(doc.body.textContent).toContain("Office words");
    expect(doc.body.innerHTML).not.toMatch(/o:p/i);
    // What a reader never sees goes: conditional comments, noscript, templates.
    expect(result).not.toContain("Outlook only");
    expect(result).not.toContain("Turn on scripts");
    expect(result).not.toContain("Never shown");
    expect(doc.querySelector("p:last-child")?.textContent).toBe("End");
  });

  it("drops SVG and MathML whole, with every script and word in them", () => {
    // Their elements keep lower-case names in an HTML document, so a set of
    // upper-case names misses them, and an unwrapped <svg><script> leaves its
    // script as visible text.
    const result = emailPreview(
      '<body><svg width="10"><script>alert(1)</script><text>svg words</text></svg>' +
        '<math><mi>x</mi><script>alert(2)</script></math><p>After</p></body>',
    );
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(result).not.toContain("alert(");
    expect(result).not.toContain("svg words");
    expect(doc.querySelectorAll("svg, math, script")).toHaveLength(0);
    expect(doc.querySelector("p")?.textContent).toBe("After");
  });

  it("drops image functions that take a bare string, whatever their prefix", () => {
    const result = emailPreview(
      '<html><head><style>' +
        '.a{background:-webkit-cross-fade("https://track.test/a.png","https://track.test/b.png",50%);color:red}' +
        '.b{background-image:cross-fade(50% "https://track.test/c.png",white);margin:0}' +
        '.c{background:-webkit-image-set("https://track.test/d.png" 1x);padding:1px}' +
        '.d{background:image("https://track.test/e.png");border:0}' +
        '.e{background:-moz-element(#banner);top:0}' +
        '</style></head><body>' +
        '<p style="background:-webkit-cross-fade(&quot;https://track.test/f.png&quot;,&quot;https://track.test/g.png&quot;,50%);color:#2b2b2b">Hi</p>' +
        '</body></html>',
    );
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(result).not.toContain("track.test");
    const css = [...doc.querySelectorAll("style")].map((style) => style.textContent).join("");
    for (const kept of [".a{color:red}", ".b{margin:0}", ".c{padding:1px}", ".d{border:0}", ".e{top:0}"]) expect(css).toContain(kept);
    expect(doc.querySelector("p")?.getAttribute("style")).toBe("color:#2b2b2b");
  });

  it("drops behavior and -moz-binding as properties, and keeps a rule that only says the word", () => {
    const result = emailPreview(
      '<html><head><style>' +
        '.behavior-note{color:blue}.binding{margin:0}' +
        '.y{behavior:url(x.htc);-moz-binding:url(#b);-ms-behavior:url(y.htc);color:red}.z{*behavior:expression(x);width:1px}' +
        // A value that names no URL: only the property says it loads.
        '.w{behavior:#default#VML;height:2px}.v{-moz-binding:none;left:0}' +
        '</style></head><body><p class="behavior-note" style="font-family:Behavior Sans;color:#2b2b2b">Hi</p></body></html>',
    );
    const doc = new DOMParser().parseFromString(result, "text/html");
    const css = [...doc.querySelectorAll("style")].map((style) => style.textContent).join("");
    expect(css).toContain(".behavior-note{color:blue}");
    expect(css).toContain(".binding{margin:0}");
    expect(css).toContain(".y{color:red}");
    expect(css).toContain(".z{width:1px}");
    expect(css).toContain(".w{height:2px}");
    expect(css).toContain(".v{left:0}");
    expect(css).not.toMatch(/htc|#b\)|expression|VML|binding:/);
    expect(doc.querySelector("p")?.getAttribute("style")).toBe("font-family:Behavior Sans;color:#2b2b2b");
  });

  it("removes meta refresh, base and link", () => {
    const result = emailPreview(
      '<html><head><meta http-equiv="refresh" content="0;url=https://evil.test/"><base href="https://evil.test/">' +
        '<link rel="stylesheet" href="https://evil.test/x.css"><title>Hello</title></head>' +
        '<body><meta http-equiv="refresh" content="1;url=https://evil.test/again"><link rel="preload" href="https://evil.test/y"><p>Hello</p></body></html>',
    );
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(result).not.toContain("evil.test");
    expect(doc.querySelectorAll("base, link")).toHaveLength(0);
    // The one meta that speaks to the browser is the preview's own policy.
    expect([...doc.querySelectorAll("meta[http-equiv]")].map((meta) => meta.getAttribute("http-equiv"))).toEqual(["Content-Security-Policy"]);
    expect(doc.querySelector("p")?.textContent).toBe("Hello");
  });

  it("its CSP allows inline styles and data images only", () => {
    const doc = new DOMParser().parseFromString(emailPreview("<p>Hello</p>"), "text/html");
    const policy = doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content") ?? "";
    const directives = new Map(
      policy
        .split(";")
        .map((part) => part.trim().split(/\s+/))
        .filter((words) => words[0] !== "")
        .map((words) => [words[0]!, words.slice(1).join(" ")] as const),
    );
    expect(directives.get("default-src")).toBe("'none'");
    expect(directives.get("style-src")).toBe("'unsafe-inline'");
    expect(directives.get("img-src")).toBe("data:");
    // Everything else it says only narrows: nothing is loaded from anywhere.
    for (const [name, value] of directives) {
      if (name !== "style-src" && name !== "img-src") expect(value, name).toBe("'none'");
    }
    // It comes first, before anything of the email's.
    expect(doc.head.firstElementChild?.getAttribute("http-equiv")).toBe("Content-Security-Policy");
  });
});
