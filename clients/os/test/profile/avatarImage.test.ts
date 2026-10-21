import { describe, expect, it } from "vitest";

import {
  AVATAR_MAX_CHARS,
  AvatarError,
  avatarFileRefusal,
  centerSquare,
  isStorableAvatar,
  smallestFit,
} from "../../src/modules/profile/avatarImage";

// The photo pipeline's decisions (memql#6220). jsdom has no canvas, so the
// pixels are a browser concern; what is tested here is everything that decides
// whether a photo is taken, how it is cropped, and which encoding is stored.

describe("the photo a person picks", () => {
  it("takes PNG, JPEG and WebP up to 5 MB, and says why it refuses anything else", () => {
    expect(avatarFileRefusal({ type: "image/png", size: 1000 })).toBe("");
    expect(avatarFileRefusal({ type: "image/jpeg", size: 5 * 1024 * 1024 })).toBe("");
    expect(avatarFileRefusal({ type: "image/webp", size: 10 })).toBe("");
    expect(avatarFileRefusal({ type: "image/svg+xml", size: 10 })).toBe("Choose a PNG, JPEG or WebP image.");
    expect(avatarFileRefusal({ type: "image/gif", size: 10 })).toBe("Choose a PNG, JPEG or WebP image.");
    expect(avatarFileRefusal({ type: "image/png", size: 5 * 1024 * 1024 + 1 })).toBe("Choose an image under 5 MB.");
    expect(avatarFileRefusal({ type: "image/png", size: 0 })).toBe("That file is empty.");
  });

  it("crops the centred square of any shape", () => {
    expect(centerSquare(400, 300)).toEqual({ sx: 50, sy: 0, side: 300 });
    expect(centerSquare(300, 400)).toEqual({ sx: 0, sy: 50, side: 300 });
    expect(centerSquare(256, 256)).toEqual({ sx: 0, sy: 0, side: 256 });
    expect(centerSquare(301, 100)).toEqual({ sx: 100, sy: 0, side: 100 });
    expect(centerSquare(0, 100).side).toBe(0);
  });
});

describe("the value that is stored", () => {
  const sized = (type: string, length: number) => `data:${type};base64,${"A".repeat(length - `data:${type};base64,`.length)}`;

  it("holds the engine's own test: an inline image of the three types, within the bound", () => {
    expect(isStorableAvatar(sized("image/webp", 120))).toBe(true);
    expect(isStorableAvatar(sized("image/jpeg", AVATAR_MAX_CHARS))).toBe(true);
    expect(isStorableAvatar(sized("image/jpeg", AVATAR_MAX_CHARS + 1))).toBe(false);
    expect(isStorableAvatar("https://example.com/me.png")).toBe(false);
    expect(isStorableAvatar("data:image/svg+xml;base64,PHN2Zz4=")).toBe(false);
    expect(isStorableAvatar("data:image/png;base64,abc def")).toBe(false);
  });

  it("stores the best WebP that fits", () => {
    const asked: string[] = [];
    const value = smallestFit((type, quality) => {
      asked.push(`${type}@${quality}`);
      return sized(type, 1000);
    });
    expect(value.startsWith("data:image/webp;")).toBe(true);
    expect(asked).toEqual(["image/webp@0.9"]);
  });

  it("steps the quality down until the photo fits", () => {
    const value = smallestFit((type, quality) => sized(type, quality > 0.65 ? AVATAR_MAX_CHARS + 500 : 2000));
    expect(value.length).toBe(2000);
  });

  it("falls back to JPEG on a browser that cannot encode WebP, rather than storing the PNG it answers with", () => {
    const value = smallestFit((type) => (type === "image/webp" ? sized("image/png", 90_000) : sized("image/jpeg", 3000)));
    expect(value.startsWith("data:image/jpeg;")).toBe(true);
  });

  it("refuses in words when nothing fits", () => {
    expect(() => smallestFit((type) => sized(type, AVATAR_MAX_CHARS + 1))).toThrow(AvatarError);
    expect(() => smallestFit((type) => sized(type, AVATAR_MAX_CHARS + 1))).toThrow("This photo could not be made small enough. Try another.");
  });
});
