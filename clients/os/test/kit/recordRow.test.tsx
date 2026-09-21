import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { RecordList, RecordRow } from "../../src/kit/RecordRow";

afterEach(cleanup);

// THE RECORD LIST'S COLUMNS LINE UP.
//
// Every row is its own grid, and its state track was `auto` -- so each row
// resolved its tracks around ITS OWN state word, and the quiet middle column
// started at a different x on every line: 1139, 1128, 1147, 1112px down one
// list of eight deployables. Nobody saw it on a cluster whose rows all said
// "Live". It was found the first time the list was rendered with the rows it
// was designed for, which is what clients/os/qa is for.
//
// WHY THIS ASSERTS A DECLARATION AND NOT PIXELS: jsdom performs no layout, so
// a test that measured would pass against the ragged stylesheet just as
// happily (the same reasoning as test/deployables/noticeOverflow.test.tsx).
// The pixels were established in a real browser over the suite's own fixture:
// before, eight rows and five different x; after, 1078px on every row, at
// 1568px and with the narrow arrangement unchanged at 560px.

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

/** The declarations of the ONE rule that starts a line with this selector. */
function ruleOf(selector: string): string {
  const at = CSS.split("\n").filter((line) => line.startsWith(`${selector} {`));
  expect(at).toHaveLength(1);
  return at[0]!;
}

describe("a record list", () => {
  it("draws the three things a row says in the classes the stylesheet lays out", () => {
    render(
      <RecordList label="Things">
        <RecordRow name="storefront" secondary="store.example.com" state="Not deployed" onOpen={() => {}} label="Open storefront">
          <span>acme</span>
        </RecordRow>
      </RecordList>,
    );
    const row = screen.getByRole("button", { name: "Open storefront" });
    expect(row.classList.contains("os-row") && row.classList.contains("os-record-row")).toBe(true);
    expect(row.querySelector(".os-record-identity")?.textContent).toContain("storefront");
    expect(row.querySelector(".os-record-summary")?.textContent).toBe("acme");
    expect(row.querySelector(".os-record-state")?.textContent).toContain("Not deployed");
  });

  it("gives the state column a floor, so every row resolves the same tracks", () => {
    const columns = /grid-template-columns: ([^;]+);/.exec(ruleOf(".os-row.os-record-row"))?.[1] ?? "";
    expect(columns).toBe("28px minmax(0, 1fr) minmax(80px, 0.45fr) minmax(8.5rem, auto) 14px");
    // The defect, named: a state track sized by each row's own word.
    expect(columns).not.toMatch(/\) auto 14px$/);
  });

  it("leaves the narrow arrangement alone: there the summary is a second line, not a column", () => {
    const narrow = CSS.slice(CSS.indexOf("@container os-record-list (max-width: 600px)"));
    expect(narrow.slice(0, narrow.indexOf("\n}\n"))).toContain(".os-row.os-record-row { grid-template-columns: 22px minmax(0, 1fr) auto 12px;");
  });
});
