// The layout planner (epic memql#4661, task memql#4664).
//
// THE INVARIANT THIS FILE EXISTS FOR: for any (layout, entry mix), every entry
// of the arrangement appears in exactly one slot. A layout that silently
// dropped an entry it had no home for would lose an element somebody
// deliberately placed -- and the page would still look deliberate, so nobody
// would report it as a bug. The parametric test at the bottom is the one that
// matters; the per-layout tests above it say what each template MEANS.

import test from "node:test";
import assert from "node:assert/strict";

import { planLayout, layoutClassName, roleClassName, slotClassName } from "../src/layout.js";
import type { LayoutSlotName } from "../src/layout.js";
import { SECTION_LAYOUTS, type Arrangement, type SectionLayout } from "../src/arrangement.js";

const CONCEPT = "v9:madeup:sensorReading";

function arrange(layout: SectionLayout | undefined, elements: Arrangement["elements"]): Arrangement {
  return { conceptId: CONCEPT, ...(layout === undefined ? {} : { layout }), elements };
}

const MIXED: Arrangement["elements"] = [
  { element: "statTile", band: "reading" },
  { element: "chart.proportion", band: "shape" },
  { element: "chart.line", band: "shape" },
  { element: "table", band: "roll" },
  { element: "detail", band: "roll" },
];

function slotOf(plan: ReturnType<typeof planLayout>, at: number): LayoutSlotName | undefined {
  return plan.slots.find((s) => s.entries.some((e) => e.at === at))?.slot;
}

// ---------------------------------------------------------------------------
// Stack is the identity
// ---------------------------------------------------------------------------

test("stack is one slot in arrangement order, and is what an absent layout means", () => {
  // The fallback every repair lands on. A regression here is a regression in
  // every view in the product, including all the ones stored before layouts
  // existed.
  const plan = planLayout(arrange(undefined, MIXED));
  assert.equal(plan.layout, "stack");
  assert.equal(plan.slots.length, 1);
  assert.equal(plan.slots[0]?.slot, "flow");
  assert.deepEqual(
    plan.slots[0]?.entries.map((e) => e.at),
    [0, 1, 2, 3, 4],
    "stack must not reorder anything",
  );
  assert.deepEqual(planLayout(arrange("stack", MIXED)), plan);
});

// ---------------------------------------------------------------------------
// What each template means
// ---------------------------------------------------------------------------

test("dashboard reads numbers, then how they divide, then the list", () => {
  const plan = planLayout(arrange("dashboard", MIXED));
  assert.deepEqual(plan.slots.map((s) => s.slot), ["header", "shapes", "roll"]);
  assert.equal(slotOf(plan, 0), "header");
  assert.equal(slotOf(plan, 1), "shapes");
  assert.equal(slotOf(plan, 2), "shapes");
  assert.equal(slotOf(plan, 3), "roll");
});

test("split puts the list left and ONE detail pane right", () => {
  const plan = planLayout(arrange("split", MIXED));
  assert.equal(slotOf(plan, 3), "roll", "the table is the list");
  assert.equal(slotOf(plan, 4), "detail", "the record pane is the right-hand side");
  // A stat strip somebody kept above the list reads as a header. Burying the
  // numbers in the left column looks broken.
  assert.equal(slotOf(plan, 0), "header");
});

test("a second detail pane is an ordinary element, not a second right-hand side", () => {
  const plan = planLayout(
    arrange("split", [
      { element: "table", band: "roll" },
      { element: "detail", band: "roll" },
      { element: "detail", band: "roll", title: "Also the record" },
    ]),
  );
  const detail = plan.slots.find((s) => s.slot === "detail");
  assert.equal(detail?.entries.length, 1);
  assert.equal(detail?.entries[0]?.at, 1);
  // ...and the third entry is still SOMEWHERE.
  assert.notEqual(slotOf(plan, 2), undefined);
});

test("focus leads with the hero and puts everything else beside it", () => {
  const plan = planLayout(
    arrange("focus", [
      { element: "statTile", band: "reading" },
      { element: "chart.line", band: "shape", role: "hero" },
      { element: "table", band: "roll" },
    ]),
  );
  assert.equal(slotOf(plan, 1), "lead", "the hero leads regardless of its position");
  assert.equal(slotOf(plan, 0), "aside");
  assert.equal(slotOf(plan, 2), "aside");
});

test("focus with no hero marked leads with the first entry rather than emptying the lead", () => {
  // sanitize guarantees a focus has a hero, so this is the belt to that
  // braces. An empty lead slot with everything crammed into the aside is a
  // broken-looking page produced by a value that was merely unusual.
  const plan = planLayout(
    arrange("focus", [
      { element: "chart.line", band: "shape" },
      { element: "table", band: "roll" },
    ]),
  );
  assert.equal(slotOf(plan, 0), "lead");
  assert.equal(slotOf(plan, 1), "aside");
});

test("gallery is a card grid with the readings as a strip, and shapes below rather than dropped", () => {
  const plan = planLayout(arrange("gallery", MIXED));
  assert.equal(slotOf(plan, 0), "header");
  assert.equal(slotOf(plan, 3), "roll");
  // A pie chart between a header strip and a card grid is neither, so shapes
  // go to the overflow BELOW. They are not dropped.
  assert.equal(slotOf(plan, 1), "flow");
  assert.equal(slotOf(plan, 2), "flow");
});

// ---------------------------------------------------------------------------
// Empty slots and empty arrangements
// ---------------------------------------------------------------------------

test("a slot with no entries is not emitted", () => {
  // A host rendering a container per empty slot gets grid gaps where nothing
  // is, and "the dashboard has a hole in it" is indistinguishable from a bug.
  const plan = planLayout(arrange("dashboard", [{ element: "table", band: "roll" }]));
  assert.deepEqual(plan.slots.map((s) => s.slot), ["roll"]);
});

test("an empty arrangement plans to no slots at all rather than to an empty grid", () => {
  const plan = planLayout(arrange("dashboard", []));
  assert.deepEqual(plan.slots, []);
  assert.equal(plan.layout, "dashboard");
});

// ---------------------------------------------------------------------------
// The invariant
// ---------------------------------------------------------------------------

test("every entry lands in exactly one slot, for every layout and every entry mix", () => {
  const mixes: Arrangement["elements"][] = [
    MIXED,
    [],
    [{ element: "table", band: "roll" }],
    [{ element: "detail", band: "roll" }],
    // A section whose author put five things in one band. Valid, unusual,
    // and it still has to render.
    [
      { element: "statTile", band: "reading" },
      { element: "statTile", band: "reading", title: "Again" },
      { element: "chart.pie", band: "reading" },
      { element: "table", band: "reading" },
      { element: "detail", band: "reading" },
    ],
    // Roles that contradict the bands.
    [
      { element: "table", band: "roll", role: "hero" },
      { element: "statTile", band: "reading", role: "supporting" },
    ],
  ];

  for (const layout of SECTION_LAYOUTS) {
    for (const [i, elements] of mixes.entries()) {
      const plan = planLayout(arrange(layout, elements));
      const landed = plan.slots.flatMap((s) => s.entries.map((e) => e.at)).sort((a, b) => a - b);
      assert.deepEqual(
        landed,
        elements.map((_, at) => at),
        `${layout} / mix ${i} lost or duplicated an entry`,
      );
      // The index is the identity a host keys on and a composer addresses.
      for (const slot of plan.slots) {
        for (const planned of slot.entries) {
          assert.equal(planned.entry, elements[planned.at]);
        }
      }
    }
  }
});

test("the class names a host applies are stable and specific", () => {
  // Composed spellings would read to the stylesheet guard as the class
  // `vk-slot-`, which is styled by nothing -- so the guard would pass while
  // checking a class that does not exist.
  assert.equal(layoutClassName("dashboard"), "vk-arrangement vk-arrangement-dashboard");
  assert.equal(slotClassName("detail"), "vk-slot vk-slot-detail");
  assert.equal(roleClassName("hero"), "vk-role-hero");
  assert.equal(roleClassName("standard"), "vk-role-standard");
  for (const layout of SECTION_LAYOUTS) {
    assert.equal(planLayout(arrange(layout, MIXED)).className, layoutClassName(layout));
  }
});
