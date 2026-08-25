// One scroll region (memql#4505).
//
// -----------------------------------------------------------------------------
// WHAT WAS MEASURED, BEFORE ANY OF THIS WAS WRITTEN
// -----------------------------------------------------------------------------
//
// The report was two vertical scrollbars, the outer one reaching up past the
// header -- i.e. the DOCUMENT scrolling, which this shell is built so it never
// can. It was not reproducible from source. The real shell was rendered
// through these same providers, its DOM served with the real built stylesheet,
// and measured in Chromium inside iframes at eight viewport heights from
// 1020px down to 400px:
//
//   document.scrollHeight == clientHeight at every height
//   the rail's floor -- everything in it that cannot shrink -- is 162px, so it
//     has room until the viewport is under about 210px
//   `min-h-full` inside `main`'s `p-6` resolves against main's CONTENT box, so
//     it lands exactly and produces no phantom overflow either
//
// A null result is worth nothing until the instrument is shown able to say
// otherwise, so the same probe was perturbed. Two perturbations, one negative
// and one positive, and the negative one matters as much:
//
//   injecting a 100vh element inside the shell   -> NO document overflow.
//        The flex column absorbs it. So "something is sized to the viewport"
//        is NOT the mechanism here, and this file deliberately does not guard
//        against it -- that would be a rule resting on a belief already
//        disproven.
//   removing min-h-0 from the rail's scroll box  -> 387px of document
//        overflow at a 700px viewport. That IS the reported symptom, exactly:
//        the rail can no longer absorb the height it was given, the nav spills,
//        and the document grows past the header.
//
// So the conclusion is that HEAD is sound and the deployed 0.19.9 bundle
// predates it -- and the thing worth keeping is not a fix but the INVARIANT,
// asserted below, whose violation was measured to reproduce the bug.
//
// -----------------------------------------------------------------------------
// WHY THESE ARE CLASS ASSERTIONS AND NOT MEASUREMENTS
// -----------------------------------------------------------------------------
//
// jsdom performs no layout: every height here would be zero. The measurement
// half lives where measurement exists, in the visual pass (memql#4511). What
// this file holds is the structural contract those measurements depend on --
// the shrink chain from the shell root to the rail's scroll box. Each link is
// asserted because a flex item that cannot shrink is what pushes a document.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Concept, Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const CONCEPTS: Concept[] = [
  {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node in the cluster",
    type: "concept",
    displayCard: { primary: "name" },
  },
];

function fakeConnection(): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => ({
      userId: "user-test",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
      displayName: "Ada Lovelace",
    })),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

async function renderShell(): Promise<HTMLElement> {
  const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
  const { container } = render(
    <MemoryRouter initialEntries={["/concepts"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the shell tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("MemQL Portal")).toBeTruthy());
  return container;
}

function tokensOf(el: Element | null): Set<string> {
  const raw = el === null ? "" : el.getAttribute("class") ?? "";
  return new Set(raw.split(/\s+/).filter((t) => t !== ""));
}

describe("the shell frame is a fixed frame, and the document never scrolls", () => {
  it("puts the shell root directly under the mount point, with no wrapper", async () => {
    // A wrapper between #root and the shell breaks the height:100% chain
    // silently: html/body/#root are 100% (styles/index.css) and the shell is
    // h-full, so an un-sized div in between makes h-full resolve against
    // `auto` and the frame stops being a frame. Providers must render no DOM.
    const container = await renderShell();
    const shell = container.firstElementChild;
    expect(shell?.tagName).toBe("DIV");
    expect(tokensOf(shell)).toContain("h-full");
    expect(tokensOf(shell)).toContain("flex-col");
  });

  it("gives the header a fixed height and the frame below it the rest", async () => {
    const container = await renderShell();
    const shell = container.firstElementChild as HTMLElement;
    const header = shell.querySelector("header[aria-label='Portal header']");
    expect(header).not.toBeNull();

    const frame = shell.children[1];
    const classes = tokensOf(frame);
    // flex-1 takes the remaining height; min-h-0 is what lets it actually
    // shrink to it. A flex item's automatic minimum size is content-based on
    // the main axis, so without min-h-0 this row is at least as tall as its
    // tallest child and the frame grows instead of scrolling.
    expect(classes).toContain("flex-1");
    expect(classes).toContain("min-h-0");
  });

  it("makes main the one scroll region for page content", async () => {
    const container = await renderShell();
    const main = container.querySelector("main");
    const classes = tokensOf(main);
    expect(classes).toContain("overflow-auto");
    expect(classes).toContain("flex-1");
    // min-w-0 is the horizontal half of the same rule: without it a wide table
    // widens the row rather than scrolling inside it.
    expect(classes).toContain("min-w-0");
  });

  it("keeps the rail's scroll box shrinkable -- the measured failure", async () => {
    // THE ASSERTION WITH EVIDENCE BEHIND IT. Removing min-h-0 from this one
    // element in a real browser produced 387px of document overflow at a
    // 700px viewport: the rail's fixed sections (the handle, the profile row,
    // the status footer) stop being absorbable, the nav spills past its
    // stretched height, and the document grows past the header -- which is the
    // screenshot that opened memql#4502.
    const container = await renderShell();
    const nav = container.querySelector("nav[aria-label='Portal sections']");
    expect(nav).not.toBeNull();
    const scroller = nav?.querySelector(".overflow-y-auto");
    expect(scroller).not.toBeNull();
    const classes = tokensOf(scroller);
    expect(classes).toContain("min-h-0");
    expect(classes).toContain("flex-1");
    expect(classes).toContain("overflow-y-auto");
  });

  it("has exactly two scroll containers in the chrome: main and the rail", async () => {
    // A third one is how "two scrollbars" becomes three. Page content may
    // still declare its own overflow-x-auto for a wide table -- that is inside
    // main, and is why this counts the CHROME rather than the document.
    const container = await renderShell();
    const shell = container.firstElementChild as HTMLElement;
    const nav = shell.querySelector("nav[aria-label='Portal sections']") as HTMLElement;
    const header = shell.querySelector("header") as HTMLElement;

    const scrollers: string[] = [];
    for (const el of [...nav.querySelectorAll("*"), ...header.querySelectorAll("*"), nav, header]) {
      for (const cls of tokensOf(el)) {
        if (cls.startsWith("overflow-") && cls.includes("auto")) scrollers.push(cls);
        if (cls.startsWith("overflow-") && cls.includes("scroll")) scrollers.push(cls);
      }
    }
    expect(scrollers).toEqual(["overflow-y-auto"]);
    expect(tokensOf(shell.querySelector("main")).has("overflow-auto")).toBe(true);
  });

  it("never lets the header or the rail scroll out of view", async () => {
    // Both are children of the fixed frame, not of main. Stated as a test
    // because "move this into main so the page scrolls as one" is a natural
    // and wrong refactor.
    const container = await renderShell();
    const main = container.querySelector("main") as HTMLElement;
    expect(main.querySelector("header[aria-label='Portal header']")).toBeNull();
    expect(main.querySelector("nav[aria-label='Portal sections']")).toBeNull();
  });
});
