# App session recording (engine half) -- implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every action a delegated app takes becomes one `v1:work:step` and one
`v1:work:observation` on the session's subrun, file contents become
content-addressed Library files, and the model's prose becomes one transcript
artifact -- replacing today's single flattened 256 KiB string.

**Architecture:** The session runner stops flattening chunks. `event` chunks are
decoded as normalized action events and handed to a SessionWriter in
`integrations/work` that writes the rows under the owner's borrowed authority
with internal origin stamped. The recording run is the subrun epic A's
`AppSessionDelegate` already opens; its id is published on the session row so the
MCP node -- a different replica -- can write `mcp` steps into the same run
through the same writer. `stdout`/`stderr` go to one content-addressed Library
file at end.

**Tech Stack:** Go 1.26, MemQL DSL, PostgreSQL + TimescaleDB, React (MemQL OS).

**Spec:** `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`
(section 4, epic B; decisions D2, D5, D12, D17).

**Issues:** epic memql#5396; tasks memql#5397, #5398, #5399, #5400, #5401. One PR.
**Branch:** `epic/app-session-recording`.

---

## Global Constraints

- **One PR for the whole epic.** Never a PR per issue.
- **Every field is additive except two.** `v1:worker:appSession.transcript` and
  `.transcriptBytes` are RETIRED, which bricks stored rows unless the retirement
  ledger records a repairing migration (memql#5199 / memql#5209). Scope the
  migration BY CONCEPT, never by key name.
- **Composite owner tier everywhere:** `@rowAuthz(owner="ownerUserId", clusterOwner)`.
  The MCP-recorded step is owned by the session's owner resolved FROM THE
  CREDENTIAL, never from the call.
- **Internal origin is allowlisted per package.** `component/mcp` and
  `component/worker`'s request-derived paths must NOT stamp it; the stamp lives in
  `integrations/work` (already allowlisted) and `component/worker`'s
  `appSessionWriteContext`.
- **Module boundaries:** `component/worker` and `component/mcp` are separate Go
  modules and cannot import `integrations/*` or `component/server`. Declare the
  narrow interface on the component side; implement it in the main module; wire it
  in `app/` under the right build tag.
- **`make test`, never `go test ./...`** -- the bare form misses the engine.
- **No emojis** anywhere.
- **Stage files by explicit path.** Never `git add -A`.

### Values, not constants (design section 5)

| Value | Default | Where |
|---|---|---|
| observation argument ceiling | 256 KiB | `integrations/work/observation.go` |
| per-content Library cap for a recorded file | 8 MiB | `component/worker/recording.go` |
| transcript artifact cap | 32 MiB | `component/worker/recording.go` |

---

## File structure

**DSL**

| File | Change |
|---|---|
| `dsl/work/concepts.memql` | `step.fingerprint` (new object); `step.stepType` description gains the action names; `observation.data` description gains the recorded keys |
| `dsl/worker/concepts.memql` | `appSession`: add `sessionRunId`, `transcriptArtifactId`, `recordedSteps`, `droppedActions`; RETIRE `transcript`, `transcriptBytes`; re-describe `transcriptTruncated` |
| `dsl/worker/mutations.memql` | `createAppSession` gains `sessionRunId`; `endAppSession` gains `transcriptArtifactId` + `transcriptTruncated`; `appendAppSessionTranscript` becomes `recordAppSessionProgress` |
| `dsl/worker/shapes.memql` | drop the two retired keys, add the four new ones |
| `dsl/library/concepts.memql` | `file.source` and `artifact.source` enums gain `app_session` |
| `dsl/library/mutations.memql` | `createLibraryFile.source` enum gains `app_session` |
| `dsl/library/queries.memql` | new `libraryFileBySha256` -- the dedup read |

**Go**

| File | Responsibility |
|---|---|
| `component/worker/recording.go` (new) | The normalized action event, the recorder seams (`SessionRecorder`, `ContentStore`), the event decoder, the action-seq gap detector, the caps |
| `component/worker/runner.go` | Route `event` chunks to the recorder; stdout/stderr to the transcript; refuse an unresolvable owner before start; store the transcript as a Library file at end |
| `component/worker/appsession_store.go` | `sessionRunId` / `transcriptArtifactId` / progress on the row; `AppendAppSessionTranscript` -> `RecordAppSessionProgress` |
| `integrations/work/session.go` (new) | `SessionWriter`: opens/uses the recording run, allocates seq, writes action + mcp + app_answer steps and their observations, records gaps, closes the run |
| `integrations/work/observation.go` | argument ceiling raised to a value; `argsRef` |
| `component/work/kind.go` | `DeriveKind` learns the six action step types |
| `component/mcp/recording.go` (new) | `AppSessionRecorder` seam + context carrier |
| `component/mcp/tool_surface.go` | record a non-protocol tool call made under an app-session bearer |
| `component/mcp/server.go` | `SetAppSessionRecorder` + per-request context injection |
| `app/appsession_content_store.go` (new, `agent`) | Content-addressed Library writer over `server.EngineLibraryStore` + the blob uploader |
| `app/mcp_app_session_recorder.go` (new) | The MCP node's recorder over `work.SessionWriter` |
| `app/integrations_worker_agent.go` | Wire the recorder + content store into the SessionRunner |
| `app/transport_mcp.go` | Wire the MCP recorder |
| `integrations/agent/worker/app_session_delegate.go` | Thread the child run id down as the recording run |
| `integrations/agent/worker/cockpitapp.go` | Carry `RecordingRunId` onto the RunSpec |
| `component/node/routing.go` + `routing_reach.go` | Broadcast `v1:worker:appSession`; record the observation exclusion |
| `clients/os/src/apps/fleet/rows.ts` + `apps/SessionPage.tsx` | Read the new fields; the Transcript panel becomes the recording summary + artifact pointer |

**Migration**

`component/database/memory-nodes/migrations/20260920000000_app_session_transcript_retired.{up,down}.sql`

---

## Task 1: The DSL surface

**Files:**
- Modify: `dsl/work/concepts.memql`, `dsl/worker/concepts.memql`,
  `dsl/worker/mutations.memql`, `dsl/worker/shapes.memql`,
  `dsl/library/concepts.memql`, `dsl/library/mutations.memql`,
  `dsl/library/queries.memql`
- Create: `component/database/memory-nodes/migrations/20260920000000_app_session_transcript_retired.{up,down}.sql`
- Modify: `component/conceptfields/concept-fields.snapshot.json` (via `make concept-snapshot`)

**Interfaces produced:**
- concept `v1:work:step` field `fingerprint object`
- concept `v1:worker:appSession` fields `sessionRunId string`, `transcriptArtifactId string`,
  `recordedSteps int`, `droppedActions int`
- mutation `recordAppSessionProgress(sessionId!, recordedSteps, droppedActions, status)`
- query `libraryFileBySha256(sha256!)`
- enum value `app_session` on `v1:library:file.source` and `v1:library:artifact.source`

- [ ] **Step 1:** Add `fingerprint object` to `concept step` in `dsl/work/concepts.memql`
      with a `@description` saying it is written once on the session's first step and
      carries `{tools, cwdDigest, variables, filesRead}`. Extend `stepType`'s
      `@description` to name `exec`, `fs_write`, `fs_read`, `fetch`, `mcp`, `app_answer`
      as the app-session action types. Extend `observation.data`'s `@description` with
      the recorded keys.
- [ ] **Step 2:** In `dsl/worker/concepts.memql`, delete `transcript` and
      `transcriptBytes` from `concept appSession`; add `sessionRunId`,
      `transcriptArtifactId`, `recordedSteps`, `droppedActions`; re-describe
      `transcriptTruncated` as "the transcript artifact does not hold the whole
      output". Add a `@relationship(type="references", field="sessionRunId", target=run, direction="outgoing")`.
- [ ] **Step 3:** In `dsl/worker/mutations.memql`, add `sessionRunId` to
      `createAppSession`; add `transcriptArtifactId` and `transcriptTruncated` to
      `endAppSession`; replace `appendAppSessionTranscript` with
      `recordAppSessionProgress`. Update `dsl/worker/shapes.memql` to match.
- [ ] **Step 4:** Add `app_session` to the `source` enum on `v1:library:file` and
      `v1:library:artifact` (concepts) and on `createLibraryFile` (mutation). Add
      `libraryFileBySha256` to `dsl/library/queries.memql`, `@actor` + owner-filtered,
      excluding archived rows, modelled on `libraryFileByUploadedFrom`.
- [ ] **Step 5:** Write the retirement migration. Up:
      `UPDATE "MemoryNodes" SET payload = payload - 'transcript' - 'transcriptBytes'
       WHERE concept = 'v1:worker:appSession' AND payload ?| array['transcript','transcriptBytes'];`
      Down: a comment saying the values are not recoverable, matching the sibling
      retirement migrations' down files.
- [ ] **Step 6:** Run `make concept-snapshot`. It REFUSES until the `retired` ledger
      covers the two drops; add the two entries naming the migration and memql#5398.
      Re-run until it writes.
- [ ] **Step 7:** Verify the tree still loads: `go run ./cmd/memqllint` (or the
      equivalent offline load) and `make test` for the DSL conformance and snapshot
      gates.
- [ ] **Step 8:** Commit.

---

## Task 2: `component/work` learns the action step types

**Files:**
- Modify: `component/work/kind.go`
- Test: `component/work/kind_test.go`

**Interfaces produced:** `work.DeriveKind` answers `deterministic` for
`exec` / `fs_write` / `fs_read` / `fetch` / `mcp` and `reasoning` for `app_answer`.

- [ ] **Step 1:** Write the failing test: each of the six types derives its kind, and
      `app_answer` is `reasoning` because the app's own answer IS intelligence.
- [ ] **Step 2:** Run it; expect `unknown step type`.
- [ ] **Step 3:** Add the cases to `DeriveKind`.
- [ ] **Step 4:** Run; expect PASS. Commit.

---

## Task 3: The recording vocabulary in `component/worker`

**Files:**
- Create: `component/worker/recording.go`, `component/worker/recording_test.go`

**Interfaces produced:**

```go
// ActionEvent is one completed action, normalized by the cockpit across apps.
type ActionEvent struct {
    Id            string         `json:"id"`
    Seq           uint64         `json:"seq"`
    Tool          string         `json:"tool"`
    Args          map[string]any `json:"args"`
    Cwd           string         `json:"cwd"`
    ExitCode      *int           `json:"exitCode"`
    IsError       bool           `json:"isError"`
    ResultDigest  string         `json:"resultDigest"`
    ResultType    string         `json:"resultType"`
    ContentInline []ActionContent `json:"contentInline"`
    Fingerprint   map[string]any `json:"fingerprint"`
}

type ActionContent struct {
    Path     string `json:"path"`
    MimeType string `json:"mimeType"`
    Bytes    []byte `json:"bytes"`
}

// StepTypeForTool maps a normalized tool name to a work-spine step type.
func StepTypeForTool(tool string) string

// SessionRecorder is the engine's recording seam. integrations/work implements it.
type SessionRecorder interface {
    OpenRecording(ctx context.Context, r RecordingOpen) (string, error)
    RecordAction(ctx context.Context, r RecordedAction) error
    RecordGap(ctx context.Context, r RecordedGap) error
    CloseRecording(ctx context.Context, r RecordingClose) error
}

// ContentStore stores bytes as a content-addressed Library file.
type ContentStore interface {
    StoreContent(ctx context.Context, req ContentRequest) (ContentResult, error)
}
```

- [ ] **Step 1:** Write failing tests for `StepTypeForTool` (the six mappings plus the
      unknown default) and for `decodeActionEvent` (a well-formed event, a malformed
      one, and one whose `exitCode` is absent vs zero).
- [ ] **Step 2:** Run; expect compile failure.
- [ ] **Step 3:** Implement the types, `StepTypeForTool`, `decodeActionEvent` and the
      caps. `exitCode` is `*int` because absent and 0 are different answers.
- [ ] **Step 4:** Run; PASS. Commit.

---

## Task 4: The `integrations/work` SessionWriter

**Files:**
- Create: `integrations/work/session.go`, `integrations/work/session_test.go`
- Modify: `integrations/work/observation.go`

**Interfaces consumed:** `worker.RecordingOpen` / `RecordedAction` / `RecordedGap` /
`RecordingClose` from Task 3.
**Interfaces produced:** `work.NewSessionWriter(engine Engine) *SessionWriter`,
satisfying both `worker.SessionRecorder` and `mcp.AppSessionRecorder`.

- [ ] **Step 1:** Write failing tests against a fake engine that records the rendered
      MemQL: N actions write N `createWorkStep` + N `createWorkObservation` calls with
      monotonic seq; a gap writes one `note` observation; `CloseRecording` writes the
      `app_answer` step and `updateWorkRun`; every write is internal-origin-stamped and
      carries the owner's actor.
- [ ] **Step 2:** Run; compile failure.
- [ ] **Step 3:** Implement. Seq comes from ONE allocator -- a read-modify-write of
      `v1:worker:appSession.recordedSteps` through `recordAppSessionProgress` -- because
      two writers (the agent replica and the MCP node) share one run and the session row
      is the only state both can see.
- [ ] **Step 4:** Raise `maxObservationArgsBytes` to the 256 KiB value and add `argsRef`:
      arguments above the ceiling spill to a content-addressed Library file and the
      observation carries its id; a failed spill records `argsTruncated` and never fails
      the call.
- [ ] **Step 5:** Run; PASS. Commit.

---

## Task 5: The session runner records

**Files:**
- Modify: `component/worker/runner.go`, `component/worker/appsession_store.go`
- Test: `component/worker/runner_test.go`

- [ ] **Step 1:** Write the failing runner tests: (a) a session of N action events writes
      N actions through a fake recorder with monotonic seq and one transcript artifact;
      (b) an out-of-order action event is dropped and a gap recorded; (c) a session whose
      owner is blank is refused BEFORE `CreateAppSession`, with
      `workspace_owner_unresolved` semantics.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement: split the collector so `event` chunks go to the recorder and
      `stdout`/`stderr` to the transcript buffer; refuse a blank owner at the top of
      `Run`; at end store the transcript through the `ContentStore` and put its file id on
      the row.
- [ ] **Step 4:** Replace `AppendAppSessionTranscript` with `RecordAppSessionProgress` in
      the store and the interface.
- [ ] **Step 5:** Run; PASS. Commit.

---

## Task 6: The recording run is the delegate's subrun

**Files:**
- Modify: `integrations/agent/worker/app_session_delegate.go`,
  `integrations/agent/worker/cockpitapp.go`, `component/worker/runner.go`
- Test: `integrations/agent/worker/app_session_delegate_test.go`

- [ ] **Step 1:** Write the failing test: the handover's child run id reaches
      `RunSpec.RecordingRunId`, so the actions land in the run the parent step's
      `childRunId` points at.
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Add `RecordingRunId` to `planner.ExecutorRequest`'s input map and to
      `RunSpec`; thread it. When it is empty the recorder OPENS a run keyed to the session.
- [ ] **Step 4:** Run; PASS. Commit.

---

## Task 7: Content-addressed Library files

**Files:**
- Create: `app/appsession_content_store.go`, `app/appsession_content_store_test.go`
- Modify: `app/integrations_worker_agent.go`

- [ ] **Step 1:** Write the failing test: two identical contents for one owner produce ONE
      `createLibraryFile` and two references; a content above the cap records
      `contentOmitted` and returns no file id without erroring.
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Implement over `server.NewEngineLibraryStore` + the blob uploader,
      modelled on `app/integrations_skills_capture.go`: sha256 the bytes, look the digest
      up with `libraryFileBySha256`, upload+create on a miss, then stamp `producedBy` with
      `stampLibraryFileProvenance`.
- [ ] **Step 4:** Wire it into the SessionRunner. Run; PASS. Commit.

---

## Task 8: MCP calls are part of the recording

**Files:**
- Create: `component/mcp/recording.go`, `component/mcp/recording_test.go`,
  `app/mcp_app_session_recorder.go`
- Modify: `component/mcp/tool_surface.go`, `component/mcp/server.go`,
  `app/transport_mcp.go`

- [ ] **Step 1:** Write the failing test: a tool call under an app-session bearer records
      one `mcp` step with the tool's arguments and the result digest; `submit` and
      `next_task` record none; a call with no app session records none.
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Implement the seam and the call site. The owner and the recording run
      come from the SESSION ROW, resolved from the credential -- never from the call.
- [ ] **Step 4:** Wire it in `app/transport_mcp.go`. Run; PASS. Commit.

---

## Task 9: Cross-node -- the rows reach the bff

**Files:**
- Modify: `component/node/routing.go`, `component/node/routing_reach.go`
- Test: `integrations/agent/worker/forward_hop_test.go`,
  `component/node/routing_reach_test.go`

- [ ] **Step 1:** Write the failing tests: `graph.node.created.v1:worker:appSession`
      forwards; a recorded session's step events forward; `v1:work:observation` is a
      RECORDED exclusion rather than silence.
- [ ] **Step 2:** Run; FAIL.
- [ ] **Step 3:** Add the `v1:worker:appSession` created/updated rules and the
      observation exclusion with its volume reason.
- [ ] **Step 4:** Add the recorded-session case to `forward_hop_test.go` using `newHop`.
- [ ] **Step 5:** Run; PASS. Commit.

---

## Task 10: MemQL OS reads the new shape

**Files:**
- Modify: `clients/os/src/apps/fleet/rows.ts`,
  `clients/os/src/apps/fleet/apps/SessionPage.tsx`
- Test: `clients/os/test/fleet/rows.test.ts`, `clients/os/test/fleet/apps.test.tsx`

- [ ] **Step 1:** Write the failing tests: the detail row reads `sessionRunId`,
      `transcriptArtifactId`, `recordedSteps`, `droppedActions`; an absent
      `recordedSteps` is `unmeasured`, never 0.
- [ ] **Step 2:** Run `make os-test`; FAIL.
- [ ] **Step 3:** Replace the Transcript panel with a Recording panel: the action count,
      the dropped count as a warning when non-zero, and the transcript artifact id when
      the session has ended.
- [ ] **Step 4:** Run; PASS. Commit.

---

## Task 11: Regenerate, verify, ship

- [ ] **Step 1:** `make concept-snapshot`, `make arch-model`, SDK regeneration, and any
      other generated artifact the changed DSL invalidates.
- [ ] **Step 2:** `make test`.
- [ ] **Step 3:** `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... go test -count=1 ./component/memql/...`
      and the other db-gated trees `scripts/ci/db-gated-packages.sh --trees` names.
- [ ] **Step 4:** `make os-test`.
- [ ] **Step 5:** Delete this plan file, open the PR, close the issues.
