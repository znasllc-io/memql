import { resourceFrom } from "./documents.js";
import { UserInputError } from "./problems.js";
import { imageSize } from "image-size";

export const MAX_IMAGE_BYTES = 8 * 1024 * 1024;
export const rasterImageName = (name: string) => /\.(png|jpe?g|gif|webp)$/i.test(name);

/** Resolve only document-local raster images. Opening a document must not send
 * an authenticated request to another cluster or a tracking request to the web. */
export function markdownImageURI(source: string, documentURI: string): string | undefined {
  try {
    const document = new URL(documentURI);
    const target = new URL(source, document);
    if (target.search || target.hash || target.username || target.password || target.port || !rasterImageName(target.pathname)) return;
    if (document.protocol === "memql-file:" && target.protocol === document.protocol && target.host === document.host) {
      if (resourceFrom(target.href).kind === "artifacts") return target.href;
    }
    if (document.protocol === "file:" && target.protocol === "file:" && target.host === document.host) {
      // Keep local images within the document directory; webview roots enforce
      // this boundary again, including the platform's filesystem handling.
      const directory = document.pathname.slice(0, document.pathname.lastIndexOf("/") + 1);
      const path = decodeURIComponent(target.pathname);
      if (!path.includes("\\") && !path.split("/").includes("..") && path.startsWith(decodeURIComponent(directory))) return target.href;
    }
  } catch { /* A malformed or external image remains visible as alt text. */ }
}

export function imageMime(name: string, bytes: Uint8Array): string {
  if (!bytes.length || bytes.length > MAX_IMAGE_BYTES) throw new UserInputError("Choose an image smaller than 8 MiB.");
  const starts = (...values: number[]) => values.every((value, i) => bytes[i] === value);
  const formats: [RegExp, string, boolean][] = [
    [/\.png$/i, "image/png", starts(137,80,78,71,13,10,26,10)],
    [/\.jpe?g$/i, "image/jpeg", starts(255,216,255)],
    [/\.gif$/i, "image/gif", /^GIF8[79]a$/.test(new TextDecoder().decode(bytes.slice(0,6)))],
    [/\.webp$/i, "image/webp", starts(82,73,70,70) && new TextDecoder().decode(bytes.slice(8,12)) === "WEBP"],
  ];
  const format = formats.find(([extension, , valid]) => extension.test(name) && valid);
  if (!format) throw new UserInputError("Choose a PNG, JPEG, GIF, or WebP image whose contents match its file type.");
  let dimensions: ReturnType<typeof imageSize>;
  try { dimensions = imageSize(bytes); } catch { throw new UserInputError("This image is incomplete or unreadable. Choose another image."); }
  if (!dimensions.width || !dimensions.height || dimensions.width * dimensions.height > 24_000_000) throw new UserInputError("Choose an image with no more than 24 million pixels.");
  return format[1];
}

export function imageMarkdown(alt: string, uri: string): string {
  return `![${alt.replace(/[\\\[\]]/g, "\\$&").replace(/[\r\n]/g, " ")}](<${uri.replace(/>/g, "%3E")}>)`;
}
