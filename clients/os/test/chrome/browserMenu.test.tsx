import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { browserMenuIsTheFeature, suppressBrowserMenu } from "../../src/chrome/browserMenu";

const collapsed = { isCollapsed: true };
const selected = { isCollapsed: false };

function targetIn(html: string, selector: string): Element {
  const view = render(<div dangerouslySetInnerHTML={{ __html: html }} />);
  const el = view.container.querySelector(selector);
  if (el === null) throw new Error(`fixture has no ${selector}`);
  return el;
}

describe("the shell's right-click rule", () => {
  it("suppresses the browser menu on an ordinary control", () => {
    const target = targetIn('<button class="os-button">Re-read</button>', "button");
    const preventDefault = vi.fn();
    suppressBrowserMenu({ target, preventDefault }, collapsed);
    // A desktop's right-click belongs to the desktop. Back / Reload / View
    // Page Source over a window is the loudest tell that this is a tab.
    expect(preventDefault).toHaveBeenCalled();
  });

  it("suppresses it on plain text and on a bare div", () => {
    for (const [html, sel] of [
      ['<p class="os-caption">15m ago</p>', "p"],
      ['<div class="os-machine"><span>Studio mini</span></div>', "span"],
    ] as const) {
      const preventDefault = vi.fn();
      suppressBrowserMenu({ target: targetIn(html, sel), preventDefault }, collapsed);
      expect(preventDefault).toHaveBeenCalled();
    }
  });

  it("LEAVES it alone in a text field, where the menu is cut/copy/paste", () => {
    // Removing it here would mean a person with no keyboard shortcuts cannot
    // paste a worker token.
    const preventDefault = vi.fn();
    suppressBrowserMenu(
      { target: targetIn('<input class="os-input" />', "input"), preventDefault },
      collapsed,
    );
    expect(preventDefault).not.toHaveBeenCalled();
  });

  it("leaves it alone in a textarea and a contenteditable, but not a disabled-editable", () => {
    const on = vi.fn();
    suppressBrowserMenu(
      { target: targetIn("<textarea></textarea>", "textarea"), preventDefault: on },
      collapsed,
    );
    expect(on).not.toHaveBeenCalled();

    const editable = vi.fn();
    suppressBrowserMenu(
      {
        target: targetIn('<div contenteditable="true"><span>x</span></div>', "span"),
        preventDefault: editable,
      },
      collapsed,
    );
    // The click lands on the CHILD, which is why the check climbs.
    expect(editable).not.toHaveBeenCalled();

    const off = vi.fn();
    suppressBrowserMenu(
      { target: targetIn('<div contenteditable="false">x</div>', "div"), preventDefault: off },
      collapsed,
    );
    expect(off).toHaveBeenCalled();
  });

  it("leaves it alone on a link, where it is copy-address", () => {
    const preventDefault = vi.fn();
    suppressBrowserMenu(
      {
        target: targetIn('<a class="os-link" href="/docs"><span>docs</span></a>', "span"),
        preventDefault,
      },
      collapsed,
    );
    expect(preventDefault).not.toHaveBeenCalled();

    // An anchor with no href is a button wearing an <a>; it offers nothing.
    const anchorless = vi.fn();
    suppressBrowserMenu(
      { target: targetIn("<a>docs</a>", "a"), preventDefault: anchorless },
      collapsed,
    );
    expect(anchorless).toHaveBeenCalled();
  });

  it("leaves it alone whenever text is selected, wherever the pointer is", () => {
    // Someone who has just selected an error message or a registration id is
    // reaching for exactly Copy. The selection can span nodes far from the
    // target, so it is decided independently of where the click landed.
    const preventDefault = vi.fn();
    suppressBrowserMenu(
      {
        target: targetIn('<span class="os-mono">v1:worker:registration:abc</span>', "span"),
        preventDefault,
      },
      selected,
    );
    expect(preventDefault).not.toHaveBeenCalled();
  });

  it("suppresses when there is no selection object at all", () => {
    // A missing getSelection means "no selection", which is the suppressing
    // answer rather than a reason to fall open.
    const preventDefault = vi.fn();
    suppressBrowserMenu({ target: targetIn("<div>x</div>", "div"), preventDefault }, null);
    expect(preventDefault).toHaveBeenCalled();
  });

  it("suppresses for a non-element target", () => {
    expect(browserMenuIsTheFeature(null, collapsed)).toBe(false);
    expect(browserMenuIsTheFeature(document, collapsed)).toBe(false);
  });
});
