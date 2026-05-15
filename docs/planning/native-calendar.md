# Native Calendar

Status: planning. Branch: `feature/native-calendar`. Five locked
architectural decisions; this doc is the single source of truth for
implementation.

---

## Goal

Build a native memQL Calendar -- the user's own calendar, with
native concepts, an agent tool surface, and (eventually) external-
calendar mirroring through the Live Knowledge integration broker.

This is NOT a wrapper around an external calendar API. Calendar data
lives as native concept rows in the memQL graph; external sync (when
shipped) projects external rows into the same native concept rows so
agent reads are uniform regardless of source.

Relationship to Live Knowledge: per the Knowledge Trust Ladder
initiative (see `docs/planning/knowledge-trust-ladder.md` on the
`feature/knowledge-trust-ladder` branch), Live Knowledge was reframed
as the integration broker. External-mirror calendars use Live
Knowledge to fetch external data; the sync automation upserts results
into native `v1:calendar:event` rows. Calendar itself is not a Live
Knowledge consumer at query time.

---

## The five concepts

All partition-scoped (workspace-level), NOT space-scoped. Calendar
events outlive the rooms they are discussed in.

### `v1:calendar:calendar`

Container. Fields:
- `id`
- `ownerId` -- user id, nullable. Null for shared calendars (visible
  to all partition participants).
- `name` -- display name
- `color` -- display color hex
- `kind` -- enum `personal` | `shared` | `externalMirror`
- `externalBindingId` -- nullable, points at a
  `v1:knowledge:liveSource` for mirror calendars; null otherwise

### `v1:calendar:event`

The anchored item. Fields:
- `id`
- `calendarId` -- parent calendar
- `title`
- `description`
- `startAt`, `endAt` -- UTC timestamps (display tz from
  `v1:identity:user.preferences.timezone`)
- `allDay` (bool)
- `location` (string)
- `status` -- enum `confirmed` | `tentative` | `cancelled`
- `visibility` -- enum `public` | `private` (per-event privacy on
  shared calendars)
- `organizerId` -- user who created/owns the event
- `recurrenceRuleId` -- nullable; set if this event is the parent of
  a recurring series
- `parentEventId` -- nullable; set if this event is a materialized
  occurrence of a recurring series
- `externalEventId` -- nullable; stable id from the external system
  for mirrored events; serves as the upsert key

### `v1:calendar:attendee`

One row per attendee per event. Fields:
- `eventId`
- `participantId` -- the `v1:identity:user` the attendee is
- `response` -- enum `accepted` | `declined` | `tentative` |
  `noResponse`
- `isOptional` (bool)

### `v1:calendar:recurrenceRule`

RRULE-shape pattern. Fields:
- `id`
- `parentEventId` -- the event this rule attaches to
- `frequency` -- enum `daily` | `weekly` | `monthly` | `yearly`
- `interval` -- int, default 1
- `byDay` -- array of weekday codes (e.g. `["mo", "we", "fr"]`)
- `count` -- nullable int (bounded series by occurrence count)
- `until` -- nullable timestamp (bounded series by date)

### `v1:calendar:reminder`

Pre-event alert. Fields:
- `id`
- `eventId`
- `minutesBefore` -- int
- `channel` -- enum `chat` | `voice` | `notification`

---

## Recurrence: materialize over compute

The parent event carries the `recurrenceRuleId`. A materialization
cron expands the next ~90 days of occurrences as actual
`v1:calendar:event` rows whose `parentEventId` points back at the
parent event. This trades storage (cheap) for query simplicity:
"what is on tomorrow" is a trivial `startAt BETWEEN ...` filter
instead of RRULE-expansion code in every read path.

Behavior:

- Cron extends the materialization window forward as time passes
  (e.g. nightly extend to maintain a rolling 90-day horizon).
- **Single-occurrence edit** (move just THIS instance of the series):
  write the change to the materialized row only; parent series stays
  unchanged.
- **Single-occurrence cancel** (skip THIS instance): set the
  materialized row to `status = cancelled`.
- **Rule edit**: re-materialize from the edit point forward;
  previously materialized future occurrences are deleted and
  recomputed.

---

## Sync architecture (pull-on-demand, one-way in v1)

```
cron / webhook -> syncExternalCalendar(bindingId) automation
  -> liveknowledge.query(sourceName=binding.liveSourceName,
                         args={since: lastSyncAt})
  -> connector hits external API, returns normalized rows
  -> upsert each into v1:calendar:event keyed on
     (calendarId, externalEventId)
  -> stamp lastSyncAt on the binding
```

Key properties:

- **Native rows are the source of truth at query time.** Agent tools
  call `calendar.list` and get graph rows. Whether they came from
  native creation or external sync is invisible at the read path.
- **Idempotent upserts.** External events have stable ids (iCal UID,
  Google event id, etc.). Repeated syncs are no-ops on unchanged
  rows.
- **Webhook acceleration is additive.** Same sync job, lower cadence
  trigger when the external system can push.
- **One-way mirror in v1.** Edits to a `kind = externalMirror`
  calendar are rejected at the mutation layer; agents calling
  `calendar.updateEvent` / `cancelEvent` / `respondToInvite` on a
  mirrored event get a typed error ("read-only mirror; edit in source
  system or migrate to a native calendar"). Bidirectional sync is
  Phase 2.

Materialized recurrences for external events come from the external
system's already-expanded form -- we do NOT re-run RRULE for synced
events. Native-only recurring events run through the materialization
cron described above.

---

## Six agent tools

Each backed by exactly one query or mutation. `calendarId` is
optional everywhere; missing defaults to the caller's primary
personal calendar.

| Tool | Backing | Use |
|------|---------|-----|
| `calendar.list` | `queryEventsInWindow(timeRange, calendarIds?, attendeeIds?)` | "What is on my schedule tomorrow?" |
| `calendar.findFreeTime` | `queryFreeSlots(duration, attendeeIds[], window)` | "When can the three of us meet for 30 min this week?" |
| `calendar.createEvent` | `mutationCreateEvent` (+ optional `recurrenceRule`) | "Schedule a 1:1 with Alice at 2pm Friday" |
| `calendar.updateEvent` | `mutationUpdateEvent` (partial) | "Move my 3pm to Friday" |
| `calendar.cancelEvent` | `mutationCancelEvent` (status=cancelled, notifies attendees) | "Cancel my 10am" |
| `calendar.respondToInvite` | `mutationUpdateAttendeeResponse` | "Decline the marketing sync" |

**Not exposed:**

- `calendar.listCalendars` -- discovery happens at attach time, not
  per-turn.
- `calendar.deleteEvent` -- hard delete is admin tooling, not
  agent-reachable.
- `calendar.subscribe` -- changes flow via the existing cognition
  stream and canvas; no dedicated subscribe tool.

**External-mirror behavior:** update / cancel / respondToInvite on a
`kind=externalMirror` calendar return a typed error in v1.

`findFreeTime` is the value-creation tool. The other five are
accelerators (a user could do them by hand in 30 seconds in a native
calendar UI). `findFreeTime` is genuinely hard for a human ("check 5
people's calendars, find overlap, propose 3 slots") and trivial for
an agent with structured access -- this is the unique-value
capability the calendar agent tool provides.

---

## Sharing model

Shared calendars inherit partition access. No separate
`v1:calendar:calendarAccess` ACL concept in v1.

- `ownerId = null` + `kind = shared` -- visible to all partition
  participants. Read access for `reader+`, write access for `writer+`.
- `ownerId = <user>` + `kind = personal` -- visible only to that
  user.
- `ownerId = <user>` + `kind = externalMirror` -- treated as personal
  in v1 (per-user mirror); shared mirrors are Phase 2.

**Per-event privacy escape valve.** An event on a shared calendar
can set `visibility = private`. Private events are visible only to
the organizer and explicit attendees, even when the calendar itself
is shared. Mirrors how `v1:copresent:canvasState` already handles
per-row privacy.

Granular delegation use cases (Alice can read but Bob can read+write
this one calendar) are real but rare; build the dedicated ACL concept
when a user need demands it. For v1 the partition role spectrum is
sufficient.

---

## Out of scope (v1)

- **User tasks / todos.** Deferred per Q6 of the originating
  brainstorm -- see the "Calendar & agenda" section of
  `docs/ROADMAP.md` for the separate concept tree (likely
  `v1:agenda:*`).
- **Bidirectional sync.** Mirrors are read-only in v1.
- **Calendar UI placement.** Where Calendar lives in the CoPresent
  app header (new top-level tile vs nested) is a UX brainstorm, not
  a memQL question.
- **Slack / Notion / Gmail / Drive sync.** Each is Phase 2 of the
  Live Knowledge connector roadmap.
- **Per-event tags / categories.** Defer until a real use case
  surfaces.

---

## Open follow-ons

Real questions but downstream of v1 ship:

- **Reminder dispatch mechanics.** `channel ∈ {chat, voice,
  notification}` is locked; how each fires at reminder-due time is
  implementation detail. Chat is likely a canvas card or cognition
  utterance; voice is TTS via the voice-agent; notification reuses
  the existing notification path. Resolve at implementation time.
- **Calendar <-> Space optional reference.** A space representing a
  meeting could carry an optional `calendarEventId`. One nullable
  foreign key on `v1:cognition:space`; doesn't change calendar's
  design. Decide when CoPresent's meeting-space flow lands.
- **Time-zone handling.** All `startAt` / `endAt` stored UTC; display
  tz from `v1:identity:user.preferences.timezone`. Worth a smoke
  test that the agent tool respects user tz when interpreting
  "tomorrow."
- **All-day events across tz boundaries.** Standard calendar gotcha;
  resolve at implementation time.
- **Series-edit semantics.** "Edit just this occurrence" vs "edit
  this and following" vs "edit the series" is a UI question backed by
  three different mutation paths. v1 ships single-occurrence edits;
  the other two are follow-on.

---

## Implementation phases

Rough sequence:

1. **Schema.** Concept files for the five concepts under
   `dsl/v1/concepts/v1/calendar/`. Struct-form queries +
   mutations for the six agent-tool backings.
2. **Recurrence materialization cron.** Background job under
   `component/calendar/` that maintains the 90-day materialization
   horizon.
3. **Six agent tools.** Tool definitions under
   `dsl/v1/tools/v1/calendar/` + wiring through the agent tool loop.
4. **Permission rules.** Partition-access checks on calendar
   mutations; mirror-readonly enforcement at the mutation layer.
5. **Reminder dispatch.** Per-channel firing logic. Cron scans
   reminders coming due in the next interval and dispatches them.
6. **External mirror sync (one-way).** First connector likely web
   fetch (for ICS endpoints) -- or punt sync entirely until real
   demand and ship the native surface first.
7. **End-to-end smoke through CoPresent.** Verify create / list /
   findFreeTime / cancel / respondToInvite paths.
