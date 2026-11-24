import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { DesktopOnly, isPhoneOrTablet } from "../../src/chrome/DesktopOnly";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const media = (coarse: boolean, hover: boolean) => (q: string) =>
  ({ matches: q === "(pointer: coarse)" ? coarse : q === "(hover: hover)" ? hover : false });

const MAC_UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15";
const WIN_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36";

it.each([
  ["an iPhone", { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Mobile/15E148", platform: "iPhone", maxTouchPoints: 5 }, media(true, false), true],
  ["an Android phone", { userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 8) Mobile Safari/537.36", platform: "Linux armv8l", maxTouchPoints: 5 }, media(true, false), true],
  ["an Android tablet", { userAgent: "Mozilla/5.0 (Linux; Android 14; SM-X710) Safari/537.36", platform: "Linux armv8l", maxTouchPoints: 10 }, media(true, false), true],
  ["an iPad posing as a Mac", { userAgent: MAC_UA, platform: "MacIntel", maxTouchPoints: 5 }, media(true, true), true],
  ["a touch-only device with a desktop user agent", { userAgent: WIN_UA, platform: "Win32", maxTouchPoints: 10 }, media(true, false), true],
  ["a Mac", { userAgent: MAC_UA, platform: "MacIntel", maxTouchPoints: 0 }, media(false, true), false],
  ["a touchscreen Windows laptop", { userAgent: WIN_UA, platform: "Win32", maxTouchPoints: 10 }, media(false, true), false],
  ["a browser with no media queries", { userAgent: WIN_UA, platform: "Win32", maxTouchPoints: 0 }, undefined, false],
] as const)("%s -> blocked: %s", (_name, nav, mm, blocked) => {
  expect(isPhoneOrTablet(nav, mm)).toBe(blocked);
});

it("shows the coming-soon screen and copies the page address", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  render(<DesktopOnly />);
  expect(screen.getByRole("heading", { name: "Coming soon to phones and tablets" })).toBeTruthy();
  expect(screen.getByText("For now memQL OS runs on a computer.")).toBeTruthy();
  expect(screen.getByText("Paste it on your computer to continue.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Copy this link" }));
  expect(writeText).toHaveBeenCalledExactlyOnceWith(window.location.href);
  expect(await screen.findByRole("button", { name: /Copied/ })).toBeTruthy();
});

it("does not say Copied when the clipboard refuses", async () => {
  const writeText = vi.fn().mockRejectedValue(new Error("denied"));
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  render(<DesktopOnly />);
  fireEvent.click(screen.getByRole("button", { name: "Copy this link" }));
  await vi.waitFor(() => expect(writeText).toHaveBeenCalled());
  await new Promise((r) => setTimeout(r, 50));
  expect(screen.queryByRole("button", { name: /Copied/ })).toBeNull();
});
