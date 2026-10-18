// Turning a photo somebody picked into the value setMyAvatar stores
// (memql#6220).
//
// THE BROWSER DOES THE WORK, SO THE ROW STAYS SMALL. The photo lives on the
// person's user row, which every sign-in reads, so it is cropped to a centred
// square and redrawn at AVATAR_SIZE before it is written: a phone photo of
// several megabytes becomes a few kilobytes. The engine bounds the stored
// value at AVATAR_MAX_CHARS and admits only an inline image of the three types
// below; this file is what makes a real photo fit comfortably inside that.
//
// THE DECISIONS ARE PURE AND TESTED, THE PIXELS ARE NOT. jsdom has no canvas,
// so the crop rectangle and the quality ladder are plain functions over
// numbers and an injected encoder, and only the thin browser adapter at the
// bottom touches an <img> and a <canvas>.

/** The stored square, in pixels: sharp at 64px on a 2x display. */
export const AVATAR_SIZE = 160;
/** The engine's bound on the stored value (dsl/identity/mutations.memql). */
export const AVATAR_MAX_CHARS = 40_000;
/** The largest file accepted before decoding: a refusal in words beats a
 *  frozen tab decoding a 60-megapixel scan. */
export const AVATAR_INPUT_MAX_BYTES = 5 * 1024 * 1024;
export const AVATAR_INPUT_TYPES = ["image/png", "image/jpeg", "image/webp"] as const;

const STORED = /^data:image\/(webp|jpeg|png);base64,[A-Za-z0-9+/]+={0,2}$/;

/** The engine's own test, so a value that would be refused never leaves. */
export function isStorableAvatar(value: string): boolean {
  return value.length <= AVATAR_MAX_CHARS && STORED.test(value);
}

/** A refusal written for the person, beside the Upload control. */
export class AvatarError extends Error {}

/** Why this file cannot be a photo, or "" when it can be tried. */
export function avatarFileRefusal(file: { type: string; size: number }): string {
  if (!(AVATAR_INPUT_TYPES as readonly string[]).includes(file.type)) return "Choose a PNG, JPEG or WebP image.";
  if (file.size > AVATAR_INPUT_MAX_BYTES) return "Choose an image under 5 MB.";
  if (file.size === 0) return "That file is empty.";
  return "";
}

/** The centred square to take from a width x height image. */
export function centerSquare(width: number, height: number): { sx: number; sy: number; side: number } {
  const side = Math.max(0, Math.min(width, height));
  return { sx: Math.floor((width - side) / 2), sy: Math.floor((height - side) / 2), side };
}

/** Encode at falling quality, WebP first, until a value fits. */
export function smallestFit(encode: (type: "image/webp" | "image/jpeg", quality: number) => string): string {
  const qualities = [0.9, 0.8, 0.7, 0.6, 0.5];
  for (const type of ["image/webp", "image/jpeg"] as const) {
    for (const quality of qualities) {
      const value = encode(type, quality);
      // A browser that cannot ENCODE a type answers with a PNG instead. Taking
      // that as success would store a lossless photo several times the size,
      // so a mismatched answer moves on to the next type.
      if (!value.startsWith(`data:${type};`)) break;
      if (isStorableAvatar(value)) return value;
    }
  }
  throw new AvatarError("This photo could not be made small enough. Try another.");
}

/** File -> stored value, in a browser. */
export async function avatarFromFile(file: File): Promise<string> {
  const refusal = avatarFileRefusal(file);
  if (refusal !== "") throw new AvatarError(refusal);
  const image = await decode(file);
  const { sx, sy, side } = centerSquare(image.width, image.height);
  if (side === 0) throw new AvatarError("That image has no pixels to use.");
  const canvas = document.createElement("canvas");
  canvas.width = AVATAR_SIZE;
  canvas.height = AVATAR_SIZE;
  const context = canvas.getContext("2d");
  if (context === null) throw new AvatarError("This browser cannot prepare a photo.");
  context.imageSmoothingQuality = "high";
  context.drawImage(image.source, sx, sy, side, side, 0, 0, AVATAR_SIZE, AVATAR_SIZE);
  return smallestFit((type, quality) => canvas.toDataURL(type, quality));
}

async function decode(file: File): Promise<{ source: CanvasImageSource; width: number; height: number }> {
  try {
    if (typeof createImageBitmap === "function") {
      const bitmap = await createImageBitmap(file);
      return { source: bitmap, width: bitmap.width, height: bitmap.height };
    }
    const url = URL.createObjectURL(file);
    try {
      const img = new Image();
      img.src = url;
      await img.decode();
      return { source: img, width: img.naturalWidth, height: img.naturalHeight };
    } finally {
      URL.revokeObjectURL(url);
    }
  } catch {
    throw new AvatarError("That image could not be read. Try another.");
  }
}
