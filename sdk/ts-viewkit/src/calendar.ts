// Calendar -- a month grid of rows placed on a date.
//
// Built against dsl/calendar/concepts.memql: startsAt / endsAt / allDay /
// title / location, which is why the slots below are start, end, allDay,
// label and detail rather than some invented general "event" shape. It is not
// a calendar-event renderer, though: any concept carrying a datetime plots,
// and the to-do concept's dueAt lands on the grid with no change here -- with
// `end` and `allDay` reported unbound, which is exactly the partial-fit case
// the fitness contract exists to express.
//
// UTC throughout (see format.ts): a row's instant is placed on the day the
// wire says it happened, not on the day the rendering host happens to be in.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import {
  MONTH_NAMES,
  WEEKDAY_NAMES,
  booleanValue,
  formatDate,
  formatTime,
  instantValue,
  scalarText,
} from "./format.js";
import {
  boundField,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

export const CALENDAR_ELEMENT: ElementSpec = {
  id: "calendar",
  title: "Calendar",
  summary: "A month grid with each row placed on its date.",
  requires: [
    {
      slot: "start",
      description: "the date each row sits on",
      kinds: ["datetime"],
      min: 1,
      // Resolved before `end`, so the more specific name wins the start slot
      // when a concept carries both.
      preferNames: [
        "startsAt",
        "startAt",
        "startTime",
        "start",
        "beginsAt",
        "scheduledAt",
        "dueAt",
        "occurredAt",
        "date",
      ],
    },
    {
      slot: "end",
      description: "the instant each row ends",
      kinds: ["datetime"],
      min: 0,
      explicitOnly: true,
      preferNames: ["endsAt", "endAt", "endTime", "end", "finishesAt", "expiresAt"],
      degraded: "each row renders as a single moment rather than a span",
    },
    {
      slot: "label",
      description: "what to call each row on the grid",
      kinds: ["text"],
      min: 0,
      prefer: ["primary"],
      degraded: "rows are labelled by their id",
    },
    {
      slot: "allDay",
      description: "whether a row is a whole-day entry",
      kinds: ["boolean"],
      min: 0,
      explicitOnly: true,
      preferNames: ["allDay", "isAllDay", "fullDay", "wholeDay"],
      degraded: "every row shows a clock time",
    },
    {
      slot: "detail",
      description: "a supporting line under each entry",
      kinds: ["text"],
      min: 0,
      explicitOnly: true,
      prefer: ["tertiary", "secondary"],
      preferNames: ["location", "place", "venue"],
      degraded: "entries show only their label",
    },
  ],
  render: (input) => draw(input),
};

interface Placed {
  readonly row: RowLike;
  readonly start: Date;
  readonly end?: Date;
}

// monthKey is the "YYYY-MM" the options accept and the grid renders.
function monthKey(d: Date): string {
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}`;
}

function parseMonth(key: string): { year: number; month: number } | undefined {
  const m = /^(\d{4})-(\d{2})$/.exec(key);
  if (!m) return undefined;
  const month = Number(m[2]) - 1;
  if (month < 0 || month > 11) return undefined;
  return { year: Number(m[1]), month };
}

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const startField = boundField(fit, "start");
  const endField = boundField(fit, "end");
  const labelField = boundField(fit, "label");
  const allDayField = boundField(fit, "allDay");
  const detailField = boundField(fit, "detail");

  const placed: Placed[] = [];
  for (const row of rows) {
    const start = instantValue(row, startField);
    if (!start) continue;
    placed.push({ row, start, end: instantValue(row, endField) });
  }
  if (placed.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text(`No dated rows for ${concept.entity}.`),
    ]);
  }

  // Which month. Explicit wins; otherwise the earliest row's, so the default
  // view always contains at least one entry.
  placed.sort((a, b) => a.start.getTime() - b.start.getTime());
  const requested = options?.month ? parseMonth(options.month) : undefined;
  const anchor = requested ?? {
    year: placed[0].start.getUTCFullYear(),
    month: placed[0].start.getUTCMonth(),
  };
  const key = `${anchor.year}-${String(anchor.month + 1).padStart(2, "0")}`;

  const byDate = new Map<string, Placed[]>();
  let outside = 0;
  for (const item of placed) {
    if (monthKey(item.start) !== key) {
      outside += 1;
      continue;
    }
    const date = formatDate(item.start);
    const list = byDate.get(date) ?? [];
    list.push(item);
    byDate.set(date, list);
  }

  const first = new Date(Date.UTC(anchor.year, anchor.month, 1));
  const daysInMonth = new Date(Date.UTC(anchor.year, anchor.month + 1, 0)).getUTCDate();
  const leading = first.getUTCDay();
  const cellCount = Math.ceil((leading + daysInMonth) / 7) * 7;

  const cells: VNode[] = [];
  for (let i = 0; i < cellCount; i += 1) {
    const dayNumber = i - leading + 1;
    if (dayNumber < 1 || dayNumber > daysInMonth) {
      cells.push(h("div", { class: "vk-cal-day vk-cal-day-blank" }, []));
      continue;
    }
    const date = formatDate(new Date(Date.UTC(anchor.year, anchor.month, dayNumber)));
    const events = byDate.get(date) ?? [];
    const children: VNode[] = [
      h("div", { class: "vk-cal-daynum" }, [text(String(dayNumber))]),
    ];
    for (const item of events) {
      children.push(
        eventChip(item, {
          labelField,
          detailField,
          allDayField,
          selectedRowId: options?.selectedRowId,
        }),
      );
    }
    cells.push(h("div", { class: "vk-cal-day", "data-vk-date": date }, children));
  }

  const body: VNode[] = [
    h("div", { class: "vk-cal-title" }, [
      text(`${MONTH_NAMES[anchor.month]} ${anchor.year}`),
    ]),
    h(
      "div",
      { class: "vk-cal-weekdays" },
      WEEKDAY_NAMES.map((name) => h("div", { class: "vk-cal-weekday" }, [text(name)])),
    ),
    h("div", { class: "vk-cal-grid" }, cells),
  ];
  if (outside > 0) {
    body.push(
      h("div", { class: "vk-cal-overflow" }, [
        text(`${outside} more outside ${MONTH_NAMES[anchor.month]} ${anchor.year}.`),
      ]),
    );
  }
  return h("div", { class: "vk-cal", "data-vk-month": key }, body);
}

function eventChip(
  item: Placed,
  ctx: {
    labelField?: string;
    detailField?: string;
    allDayField?: string;
    selectedRowId?: string;
  },
): VNode {
  const id = rowDisplayId(item.row);
  const attrs: Record<string, string> = { class: "vk-cal-event", "data-row-id": id };
  if (ctx.selectedRowId !== undefined && id === ctx.selectedRowId) {
    attrs["data-selected"] = "true";
  }

  const children: VNode[] = [];
  const allDay = booleanValue(item.row, ctx.allDayField) === true;
  if (!allDay) {
    children.push(h("span", { class: "vk-cal-time" }, [text(formatTime(item.start))]));
  }
  children.push(
    h("span", { class: "vk-cal-label" }, [
      text(scalarText(item.row, ctx.labelField) || id),
    ]),
  );
  // A span that leaves the day it started on says so rather than silently
  // occupying one cell -- the grid places rows by start, and hiding the end
  // would make a week-long row indistinguishable from an hour-long one.
  if (item.end && formatDate(item.end) !== formatDate(item.start)) {
    children.push(
      h("span", { class: "vk-cal-span" }, [text(`to ${formatDate(item.end)}`)]),
    );
  }
  const detail = scalarText(item.row, ctx.detailField);
  if (detail) {
    children.push(h("span", { class: "vk-cal-detail" }, [text(detail)]));
  }
  return h("div", attrs, children);
}

export function renderCalendar(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(CALENDAR_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}
