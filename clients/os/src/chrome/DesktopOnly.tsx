import { Button } from "../kit";
import { Mark } from "./Mark";

// MemQL OS IS OFF ON PHONES AND TABLETS FOR NOW (José, Nov 22 2026; the look
// is option 4B of the Nov 22 list). main.tsx asks this once, before anything
// renders, and draws `DesktopOnly` instead of the OS -- so no sign-in redirect,
// no curtain and no address-bar scrubbing happen, and the link a person copies
// is the whole address they arrived with.
//
// THE RULE, and why each part:
//  1. The user agent names a phone or tablet (iPhone, iPad, iPod, Android,
//     "Mobi"). Android tablets say Android without "Mobile", so Android alone.
//  2. iPadOS Safari asks for desktop sites by default and then reports itself
//     as a Mac. No Mac has a touchscreen, so a Mac platform with more than one
//     touch point is an iPad.
//  3. Anything else whose PRIMARY pointer is coarse and that cannot hover: a
//     touch-only device whose browser hides what it is.
// A touchscreen laptop or desktop is NOT blocked: its primary pointer is the
// mouse or trackpad (fine, can hover), so rule 3 does not match, and its user
// agent names no phone. Screen size is deliberately not part of the rule: a
// small desktop window is still a desktop, and a large phone is still a phone.
// The decision is made once per page load; rotating or docking does not flip it.
export function isPhoneOrTablet(
  nav: Pick<Navigator, "userAgent" | "platform" | "maxTouchPoints">,
  matchMedia?: (query: string) => { matches: boolean },
): boolean {
  if (/iPhone|iPad|iPod|Android|Mobi/i.test(nav.userAgent)) return true;
  if (/^Mac/.test(nav.platform ?? "") && (nav.maxTouchPoints ?? 0) > 1) return true;
  if (!matchMedia) return false;
  return matchMedia("(pointer: coarse)").matches && !matchMedia("(hover: hover)").matches;
}

async function copyThisLink(): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(window.location.href);
    return true;
  } catch {
    // No clipboard (an insecure origin, a refused permission): the button
    // must not say "Copied".
    return false;
  }
}

export function DesktopOnly() {
  return (
    <main className="os-desktop-only">
      <span className="os-core-gate-mark" aria-hidden>
        <Mark className="os-ask-mark" />
      </span>
      <h1 className="os-core-gate-head">Coming soon to phones and tablets</h1>
      <p className="os-core-gate-body">For now memQL OS runs on a computer.</p>
      <Button onClick={copyThisLink} doneLabel="Copied">Copy this link</Button>
      <p className="os-core-gate-note">Paste it on your computer to continue.</p>
    </main>
  );
}
