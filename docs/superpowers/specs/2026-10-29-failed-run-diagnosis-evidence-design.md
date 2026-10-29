---
title: Failed-run diagnosis reads bounded log excerpts and ends a no-repair outcome with its reason
audience: internal
status: draft
area: operate
sinceVersion: 0.31.7
owner: znas
---

# Failed-run diagnosis evidence

Owner brief, 2026-10-29. When a pipeline run fails on its first attempt, the
engine opens an owner-bound analysis goal (`work.OpenAnalysisGoal`, key
`pipeline-maintenance:<run id>`) that runs the product recipe
`znasDeliveryDiagnosis` (memql-znas, `dsl/znasdelivery/automations.memql`).
The independent PR review works on production; this diagnosis has never
produced one. Its model-availability half is memql#6375 plus an Ollama upgrade
and is out of scope here. This record covers the other half: the recipe
refuses most real failures before it asks any model. Every fork below was put
to the owner in the brainstorm of 2026-10-29 and answered.

## 1. Why

The analysis runs' approvals on the production installation (engine 0.31.7),
2026-10-26 to 2026-10-29, as counted by the session that started this work:

| Refusal | Runs |
| --- | ---: |
| "automatic diagnosis requires one failed step" | 19 |
| "no execution journal is available" | 9 |
| "the retained failure evidence exceeds the bounded diagnosis budget" | 4 |
| "the failed step has no retained log" | 2 |
| a model was unavailable (memql#6375's scope) | 18 |

Router decisions for `znasDeliveryDiagnosisPrompt` in that window: none
succeeded. 49 `feedback` approvals are open, each asking Retry or Abandon on a
refusal that a retry cannot change. A single failed test step's log was over
64 KB for, among others, the Cockpit v0.21.0 release run 64beef50.

## 2. What the code shows

1. **`logCapped` does not mean the retained log was cut.** It says the live
   log-store copy hit its line cap; "the Library copy is complete"
   (`component/pipelines/contract.go`, `StepResult.LogCapped`). The archive
   holds every line up to `ArchiveMaxBytes`, 64 MiB by default
   (`integrations/pipelinesteps/config.go`). The recipe refuses a capped step
   as "truncated".
2. **The retained log is already secret-masked.** The capture masks every
   output line with the step's resolved secret values (and the clone token)
   before any of its three copies is written: the log store, the Library
   archive and the 40-line, 16 KiB tail (`integrations/pipelinesteps/capture.go`,
   Review Focus 1). It runs on MemQL's own node on both paths: the workbench
   replica following a cluster Job (`runner.go`, `ensureCapture`) and the
   agent receiving a fleet machine's stream (`fleet.go`). The check-run tail is
   cut from the same capture and masked again at the seam. An excerpt cut from
   the archive is therefore masked as the tail is, and reading it needs no
   secret value.
3. **A log over 2 MiB fails before the 64 KiB check.** `libraryReadFile`
   refuses "UTF-8 text up to 2 MiB" (`integrations/library/history.go`).
4. **A recipe `error(...)` becomes the Retry/Abandon question.** The failure
   path (`component/automations/failure_path.go`) runs the symptom rules, which
   recognise none of these sentences, then one `classifySymptom` model call;
   an `ask` verdict parks the run on `work.FailureApproval`, whose options are
   Retry and Abandon. A template that returns a value instead ends its run
   `succeeded`: no question and no classifier call. The answer validator skips
   a goal not requested through Nexus or the API (`integrations/work/validator.go`),
   so a pipeline-requested result costs no model call either. The goal still
   waits for the owner to accept or close it, as every goal does.
5. **The recipe never reads the run's own reason.** `failureCode` and
   `failureMessage` (memql#6320) are on `pipelineRunFull`; for a run that
   failed outside its steps they are the only evidence. The maintenance input's
   `failureCode` is still the run's `refusalCode`.
6. **A body cannot map a builtin over a list.** A construct call is a statement
   of its own and a loop body's names do not leave it (memql.md, Bodies), so
   reading one log per failed step would take one hand-written named parallel
   branch per step, and could not share an unused budget between them.

## 3. Decisions

| # | Decision | Owner's choice |
| --- | --- | --- |
| D1 | Several failed steps | Excerpts of the first three failed steps that have a retained log, in run order (`seq`), sharing one budget; every other failed step listed by key, code and message; one diagnosis over all |
| D2 | Evidence size | 48 KiB of log text in total; the 64 KiB evidence check stays as the backstop |
| D3 | No failed step has a retained log | Close with the reason; no model call |
| D4 | A diagnosis that ends without a repair | Return `{repair: "none", reason}`; the run succeeds; no question |
| D5 | Reader | A generic Library builtin over several files with one shared budget (approach A, section 8) |
| D6 | Release order | Engine first; the recipe deploys only with an engine that has the builtin |

Under D2 the router's prompt floor (`core/airoute/estimate.go`: one token per
four characters plus 4,096 for the answer) is about 17K tokens, inside any
32K-window model.

## 4. The builtin (memql)

`libraryReadFileExcerpts`, declared in `dsl/library/builtins.memql` and served
by `integration.library.readFileExcerpts`. It is DSL-only (not `@sdk`) and
carries no capability gate, as `libraryReadFile` carries none.

### Inputs

| Field | Type | Bound |
| --- | --- | --- |
| `files` | `[]object!`, each `{fileId string, label string}` | 1 to 8 entries; `fileId` required, `label` echoed back unchanged |
| `patterns` | `[]string!` | 1 to 32 RE2 expressions of at most 256 bytes, each matched against one line without its line ending |
| `maxBytes` | `int!` | 1,024 to 65,536: the excerpt bytes of all files together |

A malformed input refuses the whole call before anything is read.

### Authority

Each file is read on `libraryReadFile`'s path: a signed-in subject; the row
read through `libraryFileById` under the caller, so a file the caller may not
see is unavailable; archived files and non-text media types refused; the row
read again under the file's version gate (`fileVersionGate`); bytes opened only
through `openFileBytes`, which holds the stored key to the file owner's own
Library prefix. No bytes are fetched before the row is admitted. The `label`
confers nothing: it exists so a recipe can pair a result with its step without
comparing identifiers.

### Result

`{files: [...]}`, one entry per input in input order:

- `label`, `fileId`, `name`, `version`
- `excerpt`: the selected text (below)
- `complete`: the excerpt is the whole file
- `totalBytes`, `totalLines`: what was read
- `matchedLines`: lines that matched any pattern
- `scanCapped`: the file is longer than the 64 MiB this reader reads
- `unavailable`: set instead of the fields above when the file cannot be read,
  with the sentence `libraryReadFile` gives ("file is unavailable", ...). One
  unavailable file does not fail the call.

### One file's excerpt, within its share

- When the file fits its share, it is returned whole, changed only by the NUL
  and UTF-8 repairs below; `complete` is then true.
- Otherwise, in file order:
  1. **Head:** leading lines up to 2 KiB, or a sixteenth of the share when
     that is smaller.
  2. **Matches:** every line matching a pattern, with 3 lines before and 12
     after; a match inside a window extends it; overlapping or adjacent
     windows merge. Windows are kept earliest first while their bytes fit the
     share less the head and a tail reserve of a quarter of the share; later
     matches are counted, not kept.
  3. **Tail:** the last lines that fit what the head and the windows left,
     which is at least the quarter reserved.
- A line kept by two parts is kept once. Between two kept lines that are not
  adjacent, one marker line names the omitted range:
  `[... lines 121-4812 omitted ...]`. Markers count toward the share.
- A kept line over 2 KiB is cut on a rune boundary with
  `[... N bytes cut ...]`. NUL bytes are dropped and invalid UTF-8 becomes
  U+FFFD, so the excerpt can be stored in jsonb.
- At most the first 64 MiB (the step-log archive cap) is read; `scanCapped`
  says when a file is longer, and the tail is then the end of what was read.

The reader streams: memory is bounded by about three times the share plus
one held line (64 KiB), whatever the file's size.

**Cost bounds (added after the whole-branch review).** Any signed-in caller can
call the builtin with their own file and patterns, so it bounds its own cost:
the patterns compile to at most 1,024 RE2 instructions together, a line is
matched against only its first 4 KiB, every call has a two-minute scan
deadline (a file it stops is unavailable, "this file could not be excerpted in
time"), and the version gate is released before a file's bytes are opened,
since a version's bytes are never overwritten. A label is at most 200 bytes.

### One budget across files

Max-min fair, by actual use: files are read in ascending stored size; each
receives the remaining budget divided by the files still unread, and returns
what it did not use. A small log comes back whole and the larger ones share the
rest.

### Masking

The builtin adds none (section 2, finding 2). A pipeline step's archive was
masked when it was captured; any other file is the caller's own.

## 5. The recipe (memql-znas)

`znasDeliveryDiagnosis` keeps its owner-scoped reads, its identity guards, the
structured model call, the quote check, the independent repair review and the
`repositoryChangeRequest` path. Model answers stay proposals.

1. **Guards a fix can change stay errors:** a missing, inactive or mismatched
   source; a failed run that cannot be read or does not match. Reconnecting the
   source and answering Retry can change these, so they keep asking. None
   appeared in section 1.
2. **Stops a retry cannot change return `{repair: "none", reason}`:** not the
   first completed failure; an automatically proposed change failed its own
   checks; no execution journal; no failed step with a retained log (the
   reason quotes the run's failure or refusal message, or the first failed
   step's code and message); every selected log unavailable, empty or revised;
   evidence over the 64 KiB backstop.
3. **Evidence:**

   ```
   {repository, sha, event,
    run: {refusalCode, refusalMessage, failureCode, failureMessage},
    failedStepCount,
    failedSteps: [{key, errorCode, errorMessage, exitCode}],   // up to 12, run order
    logs: [{step, complete, totalLines, matchedLines, excerpt}]} // up to 3
   ```

   The logs are the first three failed steps with a `logFileId`, ordered by
   `seq`, read in one `libraryReadFileExcerpts` call with `maxBytes: 49152`
   and the recipe's pattern list: Go test, compile and panic output,
   TypeScript and vitest, make, Docker and Python tracebacks. The `logCapped`
   refusal is removed. A log whose version is not 1 is left out as revised
   rather than failing the run.
4. **The model's no-repair outcomes return results:** not actionable (the
   model's reason); an actionable answer without a statement, regression or
   files ("The diagnosis did not give a complete repair statement, regression
   check and file list."); the file check fails ("The proposal named
   `<path>`, which is not a concrete relative path supported by an exact quote
   from this failure."); the review rejects (the reviewer's reason).
5. **Prompts:** both templates describe the evidence: the run's facts, every
   failed step's code and message, and up to three excerpts that keep the
   matching failure lines with context and the end of the log, gaps marked,
   `complete: true` meaning the whole log. "Set actionable=false when the
   excerpts do not show the cause" replaces "logs are incomplete", and "this
   single failed delivery" becomes "this failed delivery".

## 6. Tests

**memql.**

- Pure tests of the excerpt (whole file; head, windows and tail; window
  merging and extension; the budget reached mid-window; markers; long-line
  cuts; NUL and invalid UTF-8; no match at all; the scan cap) and of the
  budget split.
- A database-backed test in the style of `read_file_db_test.go`, read from a
  second engine with no shared state: no bytes streamed before authorization;
  an outsider, an archived file and a binary file are unavailable without a
  fetch; a superseded version reads the new head; one unavailable file leaves
  the others' results intact; malformed inputs refuse before any read.
- The DSL gates the tree already runs over a new builtin.

**memql-znas.** `scripts/ci/check-delivery-policy.go` reworks its maintenance
scenarios: several failures (three excerpted, one listed); no journal, no
failed step and no log as results with no model call; a large log and a
`logCapped` log diagnosed from their excerpts; a revised log excluded; a
refused read as a result; not actionable, a quote failure and a review
rejection as results carrying their reasons; an actionable diagnosis making
the same request as today. The fake reader compiles every pattern with Go's
`regexp`, so a pattern RE2 refuses fails CI rather than production.

## 7. Release and rollout

1. **memql PR** (builtin, tests, docs): lands through a train and reaches
   production with the next engine release, once memql-znas pins it
   (`ENGINE_REF` and the overlay).
2. **memql-znas PR** (recipe, prompts, harness): linted against the memql PR's
   commit through `scripts/dev/package-analysis-engine.ref`. It must not deploy
   on 0.31.7: there `use library.builtins.{ libraryReadFileExcerpts }` does not
   resolve and strict boot refuses the whole bundle. It is opened as a draft
   saying so, and merges with the `ENGINE_REF` bump to the release carrying
   the builtin; the lint ref moves to that release's commit in the same change.
3. **The 49 open approvals** stay. A Retry re-runs the failed step under the
   installed recipe while keeping the old run's completed steps; that mix is
   not verified, so abandoning them is the safe answer. An old failure is not
   diagnosed again: its goal identity is fixed and reruns are never diagnosed.
   The next first-attempt failure after the release gets the new recipe.

## 8. Alternatives rejected

- **A pipeline-bound evidence builtin** (`pipelineFailureEvidence(runId)`):
  ties evidence to the failed run natively, but repeats reads the recipe makes,
  needs a Library port in the pipelines integration and moves step selection
  out of DSL.
- **Persisting the check-run tail on each step row:** only the last 40 lines,
  which misses Go failures in the middle of a long `go test` log; only future
  runs; and it puts log text on rows that broadcast, which the driver
  deliberately avoids.
- **A one-file excerpt builtin:** forces one named parallel branch per step in
  the recipe and cannot pass a small log's unused share to the others.
- **Ending a no-repair outcome as a terminal failure:** needs a new terminal
  code in `component/work/terminal.go` and an engine release before any
  question stops.
- **A recipe-only change first, on 0.31.7:** the owner preferred one recipe PR
  and one CI run.
- **Re-masking excerpts with re-resolved secrets:** would read secret values
  outside a run to mask an archive already masked with the values the step
  had, and a rotated value would not match anyway.

## 9. Not verified here

- A real diagnosis on production: it also needs a model that answers
  (memql#6375 and the Ollama upgrade).
- Excerpt quality on real failing logs. The tests use synthetic Go,
  TypeScript, Docker and make logs; a production log such as run 64beef50's
  needs a signed-in reader.
- Retry on an approval raised by the old recipe (section 7, item 3).
