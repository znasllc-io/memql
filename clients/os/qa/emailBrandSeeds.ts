import type { Row } from "@znasllc-io/memql-sdk-core/client";

// THE BRAND QA SCENES' SEEDS (Task 24): an organization's brand kit, and a
// stand-in for `campaignBrandPreview` that draws the kit the way the article
// layout does. Public, deterministic and the repository's own: the pictures are
// neutral grey placeholders made for these scenes (a mark and two bars, a
// landscape, a person, a circle), and the words are the article layout's own
// example (component/compose/emaildoc/layouts/article.email.json).

export const LOGO =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAANwAAABACAYAAABx/hm5AAACpklEQVR42u3dwXGrMBSF4ZRACZTgjXevgExWlOB1qqADSsj+bVyCS6AEl+AS/GBGmcn4JRMjIxDy98+cVRzLyDrW5d4LvLwAAAAAAAAAAAAAAAAA2fDn7bUa1AxqB50G9YOuN+rD39rw2srMAdOMNhrn+I257tX4v42ZBH432vkBo93qzHjA/0arQ1h4TaTxvWszDWZ7ez0MuiQ026fGMQ5mHM9stm4Bo92qM/N4RrN9rGC2T334BmBns9MBSc7ZrpnoMPOx1eHHpM/oGGkd9WEt1GuarV4oQTIlkVLP+ENysdAom2Rd4tR/dMmgsF2b8tRhabM1D/5KdLetW19av7oHd5emoF2b8t3p6iUNd478kO09/ZHBfG3k4j9vPAFE21CX8+42nnTuIsbaRSYtmshjkyChu9f0UoY7RpitemC8KsIIx8ixLCS6W0uYrYoII3czjLuLCC8rhqOtG25qONnOOHabOqwUUlJWIeXERX+Z8wLSsLteUppd0oSySppMrL11CcafYohTxPsrC1A+ZYGJIVezckjbR46h8E15FL5TJy3mTtpo7aJNt3blkMFZ6jNoXqbVm5efyXBADpfjPEVICeRiuOKTJkBOhiu6LADkZriiC99AboYrurULyM1wRTcvfxlvHwz+l2bROJd7DopbjMVenhPGe2eQZHrnoPRh5ZYuQN0zRXLZ6SIWZqm3WBBGLhBectAyu9wWbiLEEAuIg+IWZ3G3yWMGhsvZcMXdCFZIKaTM3XRF3epc0kTSZAumK+phHsoCygJbMF1Rj6tS+Fb4ttN5TBXw7TmdRw4DC2cvU5YMTqs+pwvI1HhNZEfKjx0krgAA7jPe8QGjHRkNmG68z9atNoSF/Q9NzqfwmibFfVEAAAAAAAAAAAAAAAAAYBv8A4uX85c9fhZMAAAAAElFTkSuQmCC";
export const LOGO_ON_COLOUR =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAANwAAABACAYAAABx/hm5AAACo0lEQVR42u3dwW3yMBgG4I6QETICF/bICJw7RTbICNz/CyNkhIzACIzgEimV0F9VJSZ2nPA8kk+lNaR58YfthI8PAAAAAAAAAAAoRgihurfm3tp76+9tCD8N08/a6bGVIwfzgjYG5xLijb/bOJLwd9CuYTlXwYOfQaunsjCV8W/XjjTCFsLp3m4hvbGPkyPOO4etC/l1jjzvGLZzWM/ZfwAjm5EOknxmK8Vp4ddWT28mQ+DdDdO5UK8ZtjrTBMmciZR6wTeSm/OMYibrEk/9Ry8Z7GzUpkyn3GFrXnyX6P7fuvWw9at7cXRpdjRqU+5IV+cM3DXySbbP7I+cwtdGnvzXjU8AsQ1dyaPb+KHzENHXIXLSool8bSZIePqczhW4S0TYqhf6qyKCcInsC56WI2xVRBl5WKDfQ0R5WQkcWw/c3HKyXbDvNnVZqaSkqJJy5kl/CwteQDqNrreUYTdpQlGTJmHe2luXoP85gegj/r5lAZ4dTOocgZtTcjUrl7RDZB8WvvlLnoXv1JMWS0/a2NpFgpEt3y6TEmZwcj2HYPMyD9VSWGPz8jsFDkq4HOctSkooJXC7nzSBkgK362UBKC1wu174htICt+utXVBa4Ha9efmhv+MU8H/aIm08lkcJijsZd3t5ztTfp4Aka58SlL6s/A7dFi5APQpF8makizgxr5HbYkq/xYIyMkN5KUF5RrnH4JV6EyGByNAkKO7k7EN5+hdfk0AIXLGB292NYJWUSsrSQ7erW52bNDFpsoXQ7erLPCwLWBbYQujOK4btnOD1WPi28G2ky7FBGrb2mc5XDkPm2cuUSwZ9WPN7uqDQ4DWRO1J+3UHiCgB4LniXF4J2ETSYH7zvrVvtVBYOv2xy7qfHNCnuiwIAAAAAAAAAALANXxyClwildycAAAAAAElFTkSuQmCC";
export const HEADER =
  "data:image/png;base64,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";
export const PHOTO =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAGAAAABgCAYAAADimHc4AAAEWElEQVR42u1d223bQBBkCSxBJagElqASWAJLUCf8DJAgoZHAgfMyEyMwDMMBAydG8scSVAJzKxxtyZFIkTpq5h4E5kfi4zh73Nu9vd2LIuLj4e+fRCFTWCqUGrVCswf1xnlLfW0SheMgsmOFVCFXqDpIHotK31ueEQfGn0jPdI9tToxSPzv2kfiFQgEgfR+kLQsfiE97dDga0rY0EB8EYdSKsYn4XYJIbCQ+JtPxJsaI2KYBduUQ+S1W1AO17vW5g8Q/R073NagGzSZynlgh7zpjIX/uqMo5RCXNGczLxnOkgXzfhBDIBwohkA8Ugh5wA9HdmE9paq4CwQdZR7MpnKwqkDvIT4hNCiAPpA73mE3O7QRCx2FhQvVQ6P3q133z/ea6ufjyuXl78b558frVFuQ3+U/OkXOJxoP4GAHAp5Rvf9w15x8//Ed4H+QauZZhKvuYYAq0x+/q6UMh9yD4IpIxAoBFskSNvDx7czT5LeReck9kZM0ab/fT10tjxD+H3NsKLxnV+6ckn0AINXXvFxUxNfktgOoopez9Mkia1PmHjAmggbmmdLpMWDtjrCM65wxh94utfmryW4D8hKLL6z15g8Y4WaYgzwZ9BfEuAWQI3Y8ivwVoLMh2CaB02fIhs4hKCvUjk2doAUgb4GoIZfsjrB8iayiFB1zQ5LeAB2xQ4UbPBVBtCqAJAjg94PP+vgtgHSdA2P9hEN7wB3RCcxPMUAiWEAcsOGIbDhlSAB5PRWwJAJrF6Olk3FN8gGHZiWfT0dumKMMCJs8CMnwC8CgkySkAj4LyvALwYFkKvwAcX5i1VwB0xTQcXJrYaYaWhA1zbXEupyfsyfL0XgEsyRtpc4LGQZNxmSWNdREZPBHDcyTQkKTvgAflfc8lDnnALHnEoQAHfmFWHAgBr5Bmd8gcQ0mxPN1r+58lQaMPdz+rtXd7efVt7e12zQ+1/8u5co1ca4X6YSpNcP/w+3HKwcSMqNyjnaKQe1OXLkBWRpHeaor0PmGAv4wFVZqqzFiigvKA2dKaJlHb1Hy/ZfGClKJUgQySLCujW0ibaAp2TPUViO49O39HR34LaduE40MKLVdjOs475UA9Qfy4hhZsYlQ5J1ZJCaxkGUMOADh3oIAV7bOZfENCOK5o3zHOmY1qZwJ1ZGbLk6EBG4bMF4L1pGYKtw4tXSxmnA3WzhjraICJarZ08ZDi3cx2vgk/AVK8+9Dy9S7p/SPGg2n3ldnnJTMk3BEk9uF20WCYWAOnN+H2kUEm2pEk+GF3UvKp9+/4CrC7rl7f3sxVg1a+CUDeWd6dYjc91ZiZQuUR+fKuHFsZbgghVsg9IF/ekXdrW9W4haMqSd7Jjn3n9ddQOER+Qd3rOwSRKNQWEy9tTyLbD/USqWWCkLamkWuHBYJwk/g9A3VBpuMXkW+HHqwzhRJAeqmfHUfheBRGqu3saiLnKdfPCKQPsKKkly51jy17xpB647ylvpbaivkHO7bRkKCUjlYAAAAASUVORK5CYII=";
export const ICON =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAADAAAAAwCAYAAABXAvmHAAABoklEQVR42u1aS2rDMBDVEXIEHyFH8hFyBB+gYEo2pZtcoVBKoJSG9gLZlGwNJXttvZtqwlBUIQdZI8eeoYIHifXhvZE0o58xBdLd9r5yqB1ah4ND5wABOsprqWxl5kyOwMph43CMkE3FkdpY3ZI4WnvnYBnEQ1hqs5qafFOYeExIMwXxNXOo5AytdSny9cRWv9YbdQnyMDNqyeTzRCyM/DgRNGHtAgXYpIl9Y28z2jul+HlYOJprEdYKEGCjEZtCOQjBLrYws4IE2D8LQFoRZjf49PIM3+czpCYsi3WYIjZFPM/X6QS5CeuyPRJN3mzLcxOzJypW1A2HzfvnB2wfHwbLYx6WCYcTKzrTFi+rgZB8ar1QBENAa2ifyhYQWh7/799eL4jlFRJwMAMb8NEC/O9Iuu/73zz8jd9S6o5EZziuLEYCreuT90X4PVFIABQXgJYeSn4v/AuYSsAcQ0j8JBbvRsUHMvFLCdmLOfHLaS0bGtlbSvGbehXHKuIPtlQcLao43BV/vK7igkPFFZOKSz4V16wqLrrVPDVQ8dhjac9tfgDRLnMVf3BjAQAAAABJRU5ErkJggg==";

/** A whole kit: every part filled, the colours an organization might choose, and the three fixed footer links. */
export const BRAND_KIT: Record<string, unknown> = {
  theme: {
    page: "#f3f4f6",
    canvas: "#ffffff",
    primary: "#2e4a62",
    secondary: "#4a6178",
    accent: "#a8621e",
    tint: "#eef2f6",
    text: "#1f2933",
    muted: "#66737f",
    headingFont: "Georgia",
    bodyFont: "Arial",
  },
  logo: { src: LOGO, onColour: LOGO_ON_COLOUR, wide: HEADER, alt: "Acme", href: "https://acme.example" },
  signature: { photo: PHOTO, name: { text: "Dana Reyes" }, title: "Founder of Acme" },
  follow: "Follow along for more small changes.",
  social: [
    { network: "Instagram", href: "https://instagram.com/acme", icon: ICON },
    { network: "YouTube", href: "https://youtube.com/@acme", icon: ICON },
  ],
  footer: {
    lines: ["Acme, 1 Main Street, Springfield", "You are receiving this because you joined our letters."],
    links: [
      { label: "Preferences", href: "{{preferencesUrl}}" },
      { label: "Unsubscribe", href: "{{unsubscribeUrl}}" },
      { label: "View in browser", href: "{{viewUrl}}" },
    ],
  },
  reply: { heading: "Got a question about this?", button: { label: "Write to us", href: "mailto:hello@acme.example" }, note: "We read every reply." },
};

/** The same kit in colours some of its words are hard to read on: a pale accent and a pale muted. */
export const BRAND_KIT_PALE: Record<string, unknown> = {
  ...BRAND_KIT,
  theme: { ...(BRAND_KIT["theme"] as Record<string, unknown>), accent: "#f2d16b", muted: "#a7b0b8" },
};

export function brandKitRow(kit: Record<string, unknown>): Row {
  return { id: "kit-self", ownerUserId: "v1:identity:user:me", accountId: "self", kit, createdAt: "2026-11-03T15:20:00Z" };
}

type Bag = Record<string, unknown>;
const bag = (value: unknown): Bag => (value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Bag) : {});
const words = (from: Bag, key: string): string => (typeof from[key] === "string" ? (from[key] as string) : "");
const escape = (value: string) => value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
/** A picture the stand-in can draw: a data: one. An upload in a scene has nothing to draw. */
const picture = (src: string) => (src.startsWith("data:") ? src : "");

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((at) => parseInt(hex.slice(at, at + 2), 16) / 255);
  const [r, g, b] = channels.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4)) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(foreground: string, background: string): number {
  const [hi, lo] = [luminance(foreground), luminance(background)].sort((a, b) => b - a) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

/**
 * `campaignBrandPreview`, standing in: the kit drawn the way the article layout
 * draws it, and a warning for each pair of text and background it draws below
 * WCAG AA, in the engine's words.
 */
export function brandPreview(args: { kit?: Record<string, unknown> }): Row {
  const kit = bag(args.kit);
  const theme = bag(kit["theme"]);
  const colour = (key: string) => words(theme, key) || "#000000";
  const [page, canvas, primary, secondary, accent, tint, text, muted] = ["page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted"].map(colour) as string[];
  const heading = `${words(theme, "headingFont") || "Georgia"}, Georgia, serif`;
  const body = `${words(theme, "bodyFont") || "Arial"}, Arial, sans-serif`;
  const logo = bag(kit["logo"]);
  const signature = bag(kit["signature"]);
  const reply = bag(kit["reply"]);
  const footer = bag(kit["footer"]);
  const social = Array.isArray(kit["social"]) ? (kit["social"] as unknown[]).map(bag) : [];
  const lines = Array.isArray(footer["lines"]) ? (footer["lines"] as unknown[]).filter((line): line is string => typeof line === "string") : [];
  const links = Array.isArray(footer["links"]) ? (footer["links"] as unknown[]).map(bag) : [];
  const separator = typeof footer["separator"] === "string" ? (footer["separator"] as string) : "    |    ";
  const header = picture(words(logo, "wide")) || picture(words(logo, "src"));
  const name = bag(signature["name"]);

  const pairs: [string, string, number][] = [
    [primary!, tint!, 3],
    [text!, tint!, 4.5],
    ["#ffffff", accent!, 3],
    [secondary!, canvas!, 3],
    ["#ffffff", primary!, 3],
    [text!, canvas!, 4.5],
    [muted!, "#ffffff", 4.5],
    [secondary!, "#ffffff", 4.5],
  ];
  const seen = new Set<string>();
  const warnings: Row[] = [];
  for (const [fg, bg, minimum] of pairs) {
    const ratio = contrast(fg, bg);
    const key = `${fg}|${bg}|${minimum}`;
    if (ratio >= minimum || seen.has(key)) continue;
    seen.add(key);
    warnings.push({ code: "contrast", message: `Text in ${fg} on ${bg} has a contrast of ${ratio.toFixed(1)} to 1; the minimum here is ${minimum} to 1.` });
  }

  const html = `<!DOCTYPE html><html><head><style>body{margin:0;padding:0}@media (max-width:599px){.ecw{width:100%!important;min-width:0!important}}</style></head>
<body style="margin:0;background-color:${page}"><table width="100%" cellpadding="0" cellspacing="0" style="background-color:${page}"><tr><td style="padding:24px 0">
<table align="center" width="600" cellpadding="0" cellspacing="0" class="ecw" style="width:600px;min-width:600px;margin:0 auto;background-color:${canvas};font-family:${body};color:${text}">
${header ? `<tr><td><img src="${header}" alt="${escape(words(logo, "alt"))}" width="600" style="display:block;width:100%;height:auto"></td></tr>` : ""}
<tr><td style="background-color:${tint};padding:36px 40px 12px;font-family:${heading};font-size:30px;line-height:1.15;color:${primary}">Small changes, calmer mornings</td></tr>
<tr><td style="background-color:${tint};padding:8px 40px 20px;font-size:16px;line-height:1.6;color:${text}"><em>Most busy mornings go wrong in the same few places.</em> The evening ran late, something went missing, the first hour filled with small decisions.</td></tr>
<tr><td style="background-color:${tint};padding:0 40px 28px"><span style="display:inline;background-color:${accent};color:#ffffff;font-size:20px;line-height:1.5;font-weight:700;padding:2px 6px">The fix that works most often: make one decision the night before.</span></td></tr>
${reply["heading"] ? `<tr><td style="padding:32px 40px 6px;text-align:center;font-family:${heading};font-size:22px;color:${secondary}">${escape(words(reply, "heading"))}</td></tr>
<tr><td style="padding:8px 40px;text-align:center"><span style="display:inline-block;background-color:${primary};color:#ffffff;border-radius:22px;padding:11px 26px;font-size:16px">${escape(words(bag(reply["button"]), "label"))}</span></td></tr>
<tr><td style="padding:4px 40px 24px;text-align:center;font-size:14px;color:${text}">${escape(words(reply, "note"))}</td></tr>` : ""}
${signature["photo"] || name["text"] || signature["title"] ? `<tr><td style="padding:12px 40px 28px"><table cellpadding="0" cellspacing="0"><tr>
${picture(words(signature, "photo")) ? `<td style="padding-right:16px"><img src="${picture(words(signature, "photo"))}" width="64" height="64" alt="" style="display:block;border-radius:32px"></td>` : ""}
<td><div style="font-family:${heading};font-size:20px;font-weight:700;color:${text}">${escape(words(name, "text"))}</div><div style="font-size:14px;color:${muted}">${escape(words(signature, "title"))}</div></td>
</tr></table></td></tr>` : ""}
${kit["follow"] || social.length ? `<tr><td style="padding:8px 40px 4px;text-align:center;font-size:14px;color:${text}">${escape(words(kit, "follow"))}</td></tr>
<tr><td style="padding:6px 40px 24px;text-align:center">${social.map((link) => (picture(words(link, "icon")) ? `<img src="${picture(words(link, "icon"))}" width="32" height="32" alt="${escape(words(link, "network"))}" style="margin:0 6px">` : `<span style="margin:0 8px;color:${secondary}">${escape(words(link, "network"))}</span>`)).join("")}</td></tr>` : ""}
<tr><td style="background-color:#ffffff;padding:20px 24px;text-align:center;font-size:14px;line-height:22px">
<div>${links.map((link) => `<span style="color:${secondary};text-decoration:underline">${escape(words(link, "label"))}</span>`).join(`<span style="color:${secondary};white-space:pre-wrap">${escape(separator)}</span>`)}</div>
<div style="color:${muted};white-space:pre-wrap">${lines.map(escape).join("<br>")}</div>
</td></tr>
</table></td></tr></table></body></html>`;
  return { html, warnings };
}
