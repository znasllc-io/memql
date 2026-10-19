# Fleet tuning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the owner keep a chosen fleet model loaded and take requests in parallel on the cockpit's Ollama, with the advertised `max_concurrent` always equal to what Ollama was given, and record how the MemPals bake-off's winner is made resident.

**Architecture:** Two new `policy.yaml` keys (`models.resident`, `models.parallel`) flow through discovery onto `models.Info` and never onto the label. The Ollama client stamps `keep_alive` on every route and floors `num_ctx` at a working window. The managed Ollama unit becomes the source of the slot count and the worker reads it back. `memql worker models` reports what is configured and what Ollama actually holds. Two runbook tasks apply it on the owner's laptop in two stages ordered by what they disturb.

**Tech Stack:** Go 1.26 (one module, `github.com/znasllc-io/memql-cockpit`), `gopkg.in/yaml.v3`, `log/slog`, `net/http/httptest`, systemd user units, Ollama 0.34.2.

**Spec:** `docs/superpowers/specs/2026-10-18-fleet-tuning-design.md`. Read it first; decisions D1-D8 are cited below. This plan is deleted by the PR that finishes the work (`CLAUDE.md`, Repository Structure: the plans beside the specs are deleted by the PR that finishes them); the spec stays.

**How to read the code blocks.** Created files are shown whole. Modified files are shown as unified diffs (`a/` and `b/` paths from the repository root) against the tree as the previous task leaves it; an editor or `patch -p1` applies them. The blocks and the expected outputs were produced by replaying these tasks, in order, on a copy of origin/main `58ad263`: each test failed before its change and passed after it, `gofmt` and `go vet` were clean, the package suites passed, and the resulting tree is the one described here. Values that vary are written `<time>`, `<n>`, `<REV>`.

**Base.** The branch starts at origin/main `58ad263`. By 2026-10-18 origin/main had moved nine commits (PR #523, CI scripts, `memql-package.yaml`, two unrelated test files); none touches a file this plan changes, so every diff still applies after `git fetch && git rebase origin/main`. Rebase before executing, and re-check `.github/memql-pin` (Task 1 reads it).

## Global Constraints

Every task's requirements include these.

- **One module, no `go.mod` or pin change.** `go test ./...` is the whole suite. Never edit `go.mod`, `go.sum` or `.github/memql-pin`. The sibling `../memql` on this machine is stale and cannot build `internal/worker/tools`, `inference` or `worker`; Task 1 builds a pinned layout instead and every command below assumes `source /home/znas/.local/state/memql/fleet-tuning/env.sh` has run in the shell.
- **The wire contract does not move.** The model label changes in `max=` and nowhere else. `keep_alive`, `working_context` and `parallel` never become label attributes (spec D5); `TestWireContract` must stay green and untouched.
- **Nothing changes until the owner writes it.** With no `models.resident` and no `models.parallel`, no `keep_alive` is sent, no window is pinned and the ceiling is what it was (worker environment, else 1).
- **`models.parallel` is 1 to 64, `working_context` is 1 to 1048576, `keep_alive` is a Go duration, a number of seconds, `-1` or `forever`, never `0`, at most 30 days.** Both keys REPLACE on reload; they do not merge.
- **Every Ollama route that sends a model work carries `keep_alive`**: chat, vision (a chat turn), embeddings, image generation. Forever is the JSON number `-1`; the string `"-1"` is refused by Ollama.
- **A worker-side refusal is a failed call** (`CLAUDE.md`, "The stream, hardened"). Nothing in this plan refuses a call: residency settings are advice to Ollama, and a bad setting is reported and ignored.
- **The ceiling is never zero**, whichever source is garbage: the engine reads a missing `max=` as unlimited.
- **Sentences are the product.** Every problem and report sentence is asserted by a test; rewording one is a decision made in a diff.
- **Words.** `keepalive` (one word) is the liveness delta a running call sends the cluster; `keep_alive` is Ollama's residency parameter. The engine has exactly three extension words (component, integration, pack) and this plan invents no fourth. No emojis in code, comments, commits or docs.
- **Commits.** One logical change per commit, `type(scope): subject`, a body that says why, as `znas <znas@znas.io>`, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. `CLAUDE.md` and `AGENTS.md` are one file (a symlink): edit `CLAUDE.md` only. Do not push or open a PR without the owner's say-so.
- **The cockpit runs no sudo.** Commands that need it are printed for the owner, labelled as theirs.

## Review Focus

The spec implies these and no task's happy-path tests would exercise them; each is pinned by a test in the task that owns the code.

1. `keep_alive: -1` typed unquoted (YAML reads an integer), and `0`, `soon`, `9999h` typed by mistake: forever must arrive as forever, and the rest as a sentence, never as a silent default or an unload. (Tasks 2 and 3: `TestParseKeepAlive*`, `FuzzParseKeepAlive`, `TestModelsTuning_LoadsFromPolicyYAML`, `..._AnEntryThatCannotStandIsReportedAndIgnored`)
2. A call that needs more than the resident window must run at the window it asked for; clamping it down truncates a prompt. (Task 6: `TestOllamaWorkingContext`)
3. A route that leaves `keep_alive` out can start a model at Ollama's five minutes, or shorten it on an older release: chat, vision, embeddings and image generation must all state it. (Task 6: `TestOllamaKeepAlive_EveryGenerationRouteCarriesIt`)
4. Deleting the block and sending SIGHUP must return every model to defaults, and the ceiling must never reach zero whichever of policy, environment or unit is garbage. (Task 3: `TestModelsTuning_ReloadReplacesRatherThanMerges`; Task 4: `TestEffectiveParallelism`; Task 8: `TestUnitParallel`, `FuzzUnitParallel`)
5. An entry for a model that is not allowed, not installed, or served by a runtime with no `keep_alive`, and an `/api/ps` that does not answer, must be a sentence in the report and never an error that stops the command or the worker. (Task 3: problems; Task 4: `TestProbeOllama_ResidentNotesNameWhatCannotWork`; Task 9: `TestLoadedSaysWhyItCouldNotAsk`, `TestModelsTuningReport_NothingLoadedAndNothingReadable`)

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/worker/models/resident.go` (new) | `KeepAlive`, `ParseKeepAlive`, `ResidentModel` (as written), `Resident` (validated); later the helpers discovery uses | 2, 4 |
| `internal/worker/tools/models_tuning.go` (new), `policy.go` | read, validate and report `models.parallel` and `models.resident`; replace on reload | 3 |
| `internal/worker/models/discover.go`, `models.go`, `ollama.go` | `Request.Parallel/Resident`, `Discoverer.ManagedParallel`, `ParallelSource`, `Info.KeepAlive/WorkingContext`, notes | 4 |
| `internal/worker/modelinventory.go` and the six commands that built their own | `newDiscoverer`, `modelsRequest`: the one place each is built | 5, 8 |
| `internal/worker/modelcall/ollama.go`, `modalities.go`, `session.go` | stamp `keep_alive` on every route, pin the window, plumb `Info` into the client; slot-wait log lines | 6, 7 |
| `internal/worker/inference/parallel.go` (new), `stage.go`, `plan.go` | `DefaultParallel`, the unit's `OLLAMA_NUM_PARALLEL`, `UnitParallel`, `ManagedParallel` | 8 |
| `internal/worker/models/loaded.go` (new), `internal/worker/models_tuning_report.go` (new), `models_cmd.go` | `/api/ps` and the `memql worker models` tuning section | 9 |
| `docs/local-models.md`, `CLAUDE.md` | operator doc and the load-bearing rules | 10 |
| `/home/znas/.local/state/memql/fleet-tuning/*` (outside the repo) | pinned build files, census script, before/after captures | 1, 12, 13 |

---

### Task 1: A pinned test environment (no commit)

**Files:**
- Create (outside the repo): `/home/znas/.local/state/memql/fleet-tuning/go.pin.mod`, `/home/znas/.local/state/memql/fleet-tuning/go.pin.sum`, `/home/znas/.local/state/memql/fleet-tuning/env.sh`
- Create (a git worktree of the owner's `memql` repository): `/home/znas/memql-projects/memql-pin-fleet`

**Interfaces:**
- Consumes: `.github/memql-pin` (the engine commit CI builds against).
- Produces: a shell setup, `source /home/znas/.local/state/memql/fleet-tuning/env.sh`, that makes `go build`, `go vet` and `go test` resolve the engine at the pin without touching the owner's own `../memql` checkout. Every later task starts with it.

Why this task exists: `go.mod` replaces the engine with `../memql`, which on this machine is the owner's working copy, 339 commits behind the pin. Against it `internal/worker/tools`, `inference` and `worker` do not even compile (the symptom is below), and those are three of the packages this plan changes. A `-modfile` with absolute replace paths needs no change to the repository or to the owner's checkout. CI does the equivalent by checking the pin out as the sibling.

- [ ] **Step 1: See the symptom**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
unset GOFLAGS
go test ./internal/worker/inference/ -count=1 2>&1 | head -6
```

Expected:

```text
# github.com/znasllc-io/memql-cockpit/internal/worker/tools
internal/worker/tools/dispatcher.go:150:57: dispatch.GetWorkbench undefined (type *memqlv1.ToolDispatch has no field or method GetWorkbench)
internal/worker/tools/fs.go:43:26: undefined: permission.ParseReadOptions
...
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/inference [build failed]
```

- [ ] **Step 2: Check out the pinned engine as a worktree of the owner's `memql` repository (adds a worktree entry to that repository's metadata and touches nothing in its working copy; Task 13 removes it)**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
PIN=/home/znas/memql-projects/memql-pin-fleet
mkdir -p "$FT"
git -C /home/znas/memql-projects/memql worktree add --detach "$PIN" "$(cat /home/znas/memql-projects/memql-cockpit-fleet/.github/memql-pin)"
```

Expected:

```text
Preparing worktree (detached HEAD d9414cec6)
HEAD is now at d9414cec6 chore(release): prepare engine 0.29.19 for Fleet capabilities
```

- [ ] **Step 3: Write the alternate go.mod, its go.sum and the shell setup**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
PIN=/home/znas/memql-projects/memql-pin-fleet
sed "s#=> \.\./memql#=> $PIN#" /home/znas/memql-projects/memql-cockpit-fleet/go.mod > "$FT/go.pin.mod"
cp /home/znas/memql-projects/memql-cockpit-fleet/go.sum "$FT/go.pin.sum"
printf 'export FT=%s\nexport GOFLAGS="-modfile=$FT/go.pin.mod"\n' "$FT" > "$FT/env.sh"
grep -c "$PIN" "$FT/go.pin.mod"
```

Expected:

```text
25
```

- [ ] **Step 4: Prove it: build, vet and test the packages this plan changes, before changing anything**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go build ./...
go vet ./internal/worker/...
go test ./internal/worker/models/ ./internal/worker/tools/ ./internal/worker/modelcall/ ./internal/worker/inference/ ./internal/worker/ -count=1
```

Expected: no output from build or vet, then (modelcall takes about 30 s):

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/tools	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/inference	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	<time>
```

There is nothing to commit. `scripts/macos` tests need `../memql/scripts/lib/capability.sh`, which the owner's checkout has, so a bare `go test ./...` (Task 11) passes there too.

---

### Task 2: KeepAlive and the resident types

**Files:**
- Create: `internal/worker/models/resident.go`
- Test: `internal/worker/models/resident_test.go`
- Test: `internal/worker/models/fuzz_test.go` (add one target)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `models.KeepAlive{Set, Forever bool; For time.Duration}`; `models.ParseKeepAlive(s string) (KeepAlive, error)`; `(KeepAlive).Wire() any` (nil, the int `-1`, or a duration string); `(KeepAlive).String() string`; `models.MaxKeepAlive` (30 days); `models.ResidentModel{ID, KeepAlive string; WorkingContext int}` with yaml tags `id`, `keep_alive`, `working_context` (as written); `models.Resident{ID string; KeepAlive KeepAlive; WorkingContext int}` (validated).

Decision D1. Ollama reads a `keep_alive` as a Go duration string, a number of seconds, or a negative number for forever, and refuses the string `"-1"` (no unit). The owner types `-1`, `30m`, `2h`, `3600` or `forever` by hand into `policy.yaml`; this task turns that into the one value the client will send, and refuses `0` (Ollama reads it as "unload after every call") and anything past 30 days.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/models/resident_test.go`:

```go
package models

import (
	"strings"
	"testing"
	"time"
)

// resident_test.go pins how an owner's words about keeping a model loaded
// become what Ollama is told.
//
// The words are written by hand in policy.yaml, and the most natural way
// to write "never unload" is `keep_alive: -1`, which YAML reads as an
// integer. Every spelling below is one an owner will actually type.

func TestParseKeepAlive(t *testing.T) {
	tests := []struct {
		in       string
		want     KeepAlive
		wantWire any
		wantText string
	}{
		// Not said: nothing is sent and Ollama's own default stands.
		{"", KeepAlive{}, nil, ""},
		{"   ", KeepAlive{}, nil, ""},
		// Forever, in every spelling Ollama itself accepts plus the word.
		{"-1", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"-5", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"-1m", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"forever", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"Forever", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		// A Go duration, as Ollama reads one.
		{"30m", KeepAlive{Set: true, For: 30 * time.Minute}, "30m0s", "30m0s"},
		{"2h", KeepAlive{Set: true, For: 2 * time.Hour}, "2h0m0s", "2h0m0s"},
		{"90s", KeepAlive{Set: true, For: 90 * time.Second}, "1m30s", "1m30s"},
		// A bare positive integer is SECONDS, as Ollama reads a number.
		{"3600", KeepAlive{Set: true, For: time.Hour}, "1h0m0s", "1h0m0s"},
	}
	for _, tt := range tests {
		got, err := ParseKeepAlive(tt.in)
		if err != nil {
			t.Errorf("ParseKeepAlive(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseKeepAlive(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if w := got.Wire(); w != tt.wantWire {
			t.Errorf("ParseKeepAlive(%q).Wire() = %#v, want %#v", tt.in, w, tt.wantWire)
		}
		if s := got.String(); s != tt.wantText {
			t.Errorf("ParseKeepAlive(%q).String() = %q, want %q", tt.in, s, tt.wantText)
		}
	}
}

// TestParseKeepAliveRefusesWhatWouldSilentlyDoTheWrongThing. Zero is the
// one that matters: Ollama reads 0 as "unload after every call", the
// opposite of resident, and an owner who typed it meant something else.
func TestParseKeepAliveRefusesWhatWouldSilentlyDoTheWrongThing(t *testing.T) {
	tests := []struct {
		in   string
		want string // a phrase the sentence must carry
	}{
		{"0", "unloads the model after every call"},
		{"0s", "unloads the model after every call"},
		{"00", "unloads the model after every call"},
		{"soon", "not a duration"},
		{"5 minutes", "not a duration"},
		{"1.5", "not a duration"},
		// A finite value past the cap is nearly forever and almost
		// certainly a typo for -1; saying so beats picking one.
		{"9999h", "longer than 30 days"},
		{"99999999999", "longer than 30 days"},
	}
	for _, tt := range tests {
		got, err := ParseKeepAlive(tt.in)
		if err == nil {
			t.Errorf("ParseKeepAlive(%q) = %+v, want a refusal", tt.in, got)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ParseKeepAlive(%q) error %q does not say %q", tt.in, err, tt.want)
		}
		if got.Set {
			t.Errorf("ParseKeepAlive(%q) refused but still returned a setting: %+v", tt.in, got)
		}
	}
}
```

Modify `internal/worker/models/fuzz_test.go` (shown as a diff; add exactly this):

```diff
--- a/internal/worker/models/fuzz_test.go
+++ b/internal/worker/models/fuzz_test.go
@@ -3,6 +3,7 @@
 import (
 	"strings"
 	"testing"
+	"time"
 )
 
 // FuzzAttributesRoundTrip. The label value is the ONE string this
@@ -132,3 +133,53 @@
 		}
 	})
 }
+
+// FuzzParseKeepAlive. The text is an owner's, typed by hand into
+// policy.yaml, and the failure it can cause is not a crash: it is Ollama
+// being sent a keep_alive it refuses, or one that means something the owner
+// did not write. Two properties.
+//
+//   - A SETTING THAT STANDS IS ONE OLLAMA READS THE SAME WAY. When the text
+//     parses, Wire() is the JSON number -1 or a duration string that Go's own
+//     parser reads back as the same positive duration, within the cap.
+//     Anything else is a request that fails on somebody's GPU.
+//
+//   - A REFUSAL STANDS FOR NOTHING. When the text is refused the result is
+//     the zero KeepAlive, so a typo can never be mistaken for an owner who
+//     asked for forever.
+func FuzzParseKeepAlive(f *testing.F) {
+	for _, seed := range []string{
+		"", "-1", "forever", "Forever", "30m", "2h", "3600", "0", "00", "0s", "-1m",
+		"9999h", "1.5", "soon", "1h30m", "+5m", " 5m ", "5M", "-0", "1e3",
+		"99999999999999999999", "9223372036854775807", "-9223372036854775808",
+		"720h", "720h1ns", "1ns",
+	} {
+		f.Add(seed)
+	}
+	f.Fuzz(func(t *testing.T, s string) {
+		k, err := ParseKeepAlive(s)
+		if err != nil {
+			if k != (KeepAlive{}) {
+				t.Fatalf("ParseKeepAlive(%q) was refused (%v) but returned %+v", s, err, k)
+			}
+			return
+		}
+		switch w := k.Wire().(type) {
+		case nil:
+			if k.Set {
+				t.Fatalf("ParseKeepAlive(%q) = %+v is set but sends nothing", s, k)
+			}
+		case int:
+			if w != -1 || !k.Forever {
+				t.Fatalf("ParseKeepAlive(%q) = %+v sends the number %d", s, k, w)
+			}
+		case string:
+			d, perr := time.ParseDuration(w)
+			if perr != nil || d <= 0 || d > MaxKeepAlive || d != k.For {
+				t.Fatalf("ParseKeepAlive(%q) = %+v sends %q, which Go reads as %v (%v)", s, k, w, d, perr)
+			}
+		default:
+			t.Fatalf("ParseKeepAlive(%q) sends %T", s, w)
+		}
+	})
+}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run 'TestParseKeepAlive|FuzzParseKeepAlive' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/models [build failed]
internal/worker/models/fuzz_test.go:160:13: undefined: ParseKeepAlive
internal/worker/models/fuzz_test.go:162:13: undefined: KeepAlive
internal/worker/models/fuzz_test.go:178:36: undefined: MaxKeepAlive
```

- [ ] **Step 3: Write the implementation**

Create `internal/worker/models/resident.go`:

```go
package models

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// models.resident: the models this machine keeps loaded.
//
// Ollama unloads a model five minutes after its last call unless told
// otherwise, and reloads it (about three seconds from a warm disk cache,
// forty from a cold one, on the 27B this fleet serves) the next time
// anybody asks. Two things cause a reload, and a resident model has to
// answer both: the idle timeout, which keep_alive moves, and a change in
// the requested working context, which the working window pins. See
// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

// MaxKeepAlive is the longest finite keep-alive accepted. Past it the
// owner almost certainly meant -1, and a value that is nearly forever but
// not quite is the kind of setting that is found a month later.
const MaxKeepAlive = 30 * 24 * time.Hour

// KeepAlive is a validated keep_alive setting: how long Ollama keeps a
// model loaded after a call.
//
// The zero value is "the owner said nothing", and it is not the same as a
// zero duration: nothing is sent and the runtime's own default
// (OLLAMA_KEEP_ALIVE, five minutes unless somebody changed it) applies.
type KeepAlive struct {
	// Set is false when no keep_alive is configured.
	Set bool
	// Forever keeps the model loaded until Ollama stops or another model
	// needs the space. It is not a pin: an idle model is still evicted
	// when a different one must load and does not fit.
	Forever bool
	// For is the finite duration, when Set and not Forever.
	For time.Duration
}

var errKeepAliveZero = errors.New("keep_alive 0 unloads the model after every call, which is the opposite of resident; delete the setting to get Ollama's default, or use -1 to keep the model loaded")

// ParseKeepAlive reads what an owner wrote under models.resident.
//
// Accepted, as Ollama itself reads them: a Go duration ("30m", "2h"), a
// number of seconds ("3600"), and -1 or any negative value for forever.
// "forever" is also accepted because `-1` reads as a typo to the next
// person. Zero is refused by name, and so is anything unreadable:
// returning the zero KeepAlive for garbage would make a typo
// indistinguishable from an owner who asked for nothing.
func ParseKeepAlive(s string) (KeepAlive, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return KeepAlive{}, nil
	}
	if strings.EqualFold(s, "forever") {
		return KeepAlive{Set: true, Forever: true}, nil
	}
	// A bare integer is seconds, the way Ollama reads a JSON number.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n < 0:
			return KeepAlive{Set: true, Forever: true}, nil
		case n == 0:
			return KeepAlive{}, errKeepAliveZero
		case n > int64(MaxKeepAlive/time.Second):
			return KeepAlive{}, errKeepAliveTooLong(s)
		}
		return KeepAlive{Set: true, For: time.Duration(n) * time.Second}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return KeepAlive{}, fmt.Errorf("keep_alive %q is not a duration like 30m or 2h, a number of seconds, or -1 for forever", s)
	}
	switch {
	case d < 0:
		return KeepAlive{Set: true, Forever: true}, nil
	case d == 0:
		return KeepAlive{}, errKeepAliveZero
	case d > MaxKeepAlive:
		return KeepAlive{}, errKeepAliveTooLong(s)
	}
	return KeepAlive{Set: true, For: d}, nil
}

func errKeepAliveTooLong(s string) error {
	return fmt.Errorf("keep_alive %q is longer than 30 days; use -1 to keep the model loaded until Ollama stops", s)
}

// Wire is the value for the request's keep_alive key, or nil when the key
// must be left out. Forever is the JSON number -1 and a duration is a
// string: Ollama reads "-1" as a string with no unit and refuses it, so
// the two cannot share a spelling.
func (k KeepAlive) Wire() any {
	switch {
	case !k.Set:
		return nil
	case k.Forever:
		return -1
	default:
		return k.For.String()
	}
}

// String is the setting as `memql worker models` prints it.
func (k KeepAlive) String() string {
	switch {
	case !k.Set:
		return ""
	case k.Forever:
		return "forever"
	default:
		return k.For.String()
	}
}

// ResidentModel is one entry of policy.yaml models.resident, as written.
//
//	models:
//	  resident:
//	    - id: qwen3.8:27b
//	      keep_alive: 2h
//	      working_context: 16384
//
// KeepAlive is a string so that `keep_alive: -1`, `keep_alive: 30m` and
// `keep_alive: 3600` all decode: yaml.v3 stores a scalar's own text in a
// string field. Validation is ParseKeepAlive's, and the policy reader
// turns each entry into a Resident.
type ResidentModel struct {
	ID             string `yaml:"id"`
	KeepAlive      string `yaml:"keep_alive"`
	WorkingContext int    `yaml:"working_context"`
}

// Resident is a ResidentModel after validation: the shape discovery
// applies. A field the owner left out, or one that failed validation, is
// its zero value, which means "send nothing for this".
type Resident struct {
	ID        string
	KeepAlive KeepAlive
	// WorkingContext is the num_ctx floor in tokens. A call that needs no
	// more than this runs at exactly this window, so the runner Ollama
	// loaded for one call is the runner the next call finds. Zero leaves
	// the engine's own window alone.
	WorkingContext int
}
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run 'TestParseKeepAlive|FuzzParseKeepAlive' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestParseKeepAlive (0.00s)
--- PASS: TestParseKeepAliveRefusesWhatWouldSilentlyDoTheWrongThing (0.00s)
--- PASS: FuzzParseKeepAlive (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	0.003s
```

- [ ] **Step 5: Run the package, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/models/
go test ./internal/worker/models/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/models/resident.go \
  internal/worker/models/resident_test.go \
  internal/worker/models/fuzz_test.go
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(models): validated keep_alive and the resident-model types" \
  -m "Ollama reads a keep_alive as a duration string, a number of seconds, or a negative number for forever, and refuses the string \"-1\". ParseKeepAlive turns what an owner types into the one value Wire sends, refuses 0 (which unloads after every call) and anything past 30 days, and a fuzz target holds the round trip." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: models.parallel and models.resident in policy.yaml

**Files:**
- Create: `internal/worker/tools/models_tuning.go`
- Modify: `internal/worker/tools/policy.go` (ModelsPolicy fields and reload)
- Test: `internal/worker/tools/models_tuning_policy_test.go`

**Interfaces:**
- Consumes: `models.ParseKeepAlive`, `models.ResidentModel`, `models.Resident` (Task 2).
- Produces: `tools.ModelsTuning{Parallel int; Resident []models.Resident}`; `(*Policy).ModelsTuning() ModelsTuning`; `(*Policy).ModelsTuningProblems() []string`; `tools.MaxModelsParallel` (64); `tools.MaxWorkingContext` (1048576). A nil policy and the default policy tune nothing and report nothing.

Both keys REPLACE on reload (the file is the whole truth, so deleting the block and sending SIGHUP returns every model to defaults). An entry that cannot stand is reported and ignored; a typo costs the one setting and never the machine. The sentences are asserted by the tests below.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/tools/models_tuning_policy_test.go`:

```go
package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

func policyFromDoc(t *testing.T, doc string) (*Policy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p, path
}

// TestModelsTuning_LoadsFromPolicyYAML pins the shape the docs promise.
//
// `keep_alive: -1` is written UNQUOTED on purpose: it is the way an owner
// will actually type "never unload", YAML reads it as an integer, and it
// has to arrive as forever rather than as a decode error or as nothing.
func TestModelsTuning_LoadsFromPolicyYAML(t *testing.T) {
	p, _ := policyFromDoc(t, `models:
  allow:
    - qwen3.8:27b
    - qwen3-embedding:0.6b
  parallel: 2
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
    - id: qwen3-embedding:0.6b
      keep_alive: -1
`)
	got := p.ModelsTuning()
	if got.Parallel != 2 {
		t.Errorf("parallel = %d, want 2", got.Parallel)
	}
	want := []models.Resident{
		{ID: "qwen3.8:27b", KeepAlive: models.KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384},
		{ID: "qwen3-embedding:0.6b", KeepAlive: models.KeepAlive{Set: true, Forever: true}},
	}
	if len(got.Resident) != len(want) {
		t.Fatalf("resident = %+v, want %+v", got.Resident, want)
	}
	for i := range want {
		if got.Resident[i] != want[i] {
			t.Errorf("resident[%d] = %+v, want %+v", i, got.Resident[i], want[i])
		}
	}
	if problems := p.ModelsTuningProblems(); len(problems) != 0 {
		t.Errorf("a clean file reported problems: %v", problems)
	}
}

// TestModelsTuning_AbsentIsTheDefault. Nothing here changes what a machine
// does until its owner writes it: no keep_alive is sent, no window is
// pinned, no ceiling is claimed.
func TestModelsTuning_AbsentIsTheDefault(t *testing.T) {
	for name, p := range map[string]*Policy{"default": DefaultPolicy(), "nil": nil} {
		got := p.ModelsTuning()
		if got.Parallel != 0 || len(got.Resident) != 0 {
			t.Errorf("%s policy tunes %+v, want nothing", name, got)
		}
		if problems := p.ModelsTuningProblems(); len(problems) != 0 {
			t.Errorf("%s policy reports problems: %v", name, problems)
		}
	}
}

// TestModelsTuning_ReloadReplacesRatherThanMerges.
//
// Review focus: deleting the block and sending SIGHUP must put every model
// back on Ollama's default without a restart. A merge, which the allow
// lists do, would keep a model loaded for two hours on the strength of a
// line the owner already deleted.
func TestModelsTuning_ReloadReplacesRatherThanMerges(t *testing.T) {
	p, path := policyFromDoc(t, `models:
  parallel: 3
  resident:
    - id: qwen3.8:27b
      keep_alive: -1
`)
	if got := p.ModelsTuning(); got.Parallel != 3 || len(got.Resident) != 1 {
		t.Fatalf("before reload: %+v", got)
	}
	if err := os.WriteFile(path, []byte("models:\n  allow:\n    - qwen3.8:27b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := p.ModelsTuning(); got.Parallel != 0 || len(got.Resident) != 0 {
		t.Errorf("after the block was deleted the policy still tunes %+v", got)
	}
}

// TestModelsTuning_AnEntryThatCannotStandIsReportedAndIgnored. Every
// refusal is a sentence an owner can act on, and none of them stops the
// worker or the entries beside it: a typo must cost the one setting, not
// the machine.
func TestModelsTuning_AnEntryThatCannotStandIsReportedAndIgnored(t *testing.T) {
	tests := []struct {
		name         string
		doc          string
		wantProblem  string
		wantParallel int
		wantResident int
	}{
		{
			name:         "parallel below the range",
			doc:          "models:\n  parallel: -2\n",
			wantProblem:  "models.parallel: -2 is outside 1 to 64",
			wantParallel: 0,
		},
		{
			name:         "parallel above the range",
			doc:          "models:\n  parallel: 500\n",
			wantProblem:  "models.parallel: 500 is outside 1 to 64",
			wantParallel: 0,
		},
		{
			name:         "entry without an id",
			doc:          "models:\n  resident:\n    - keep_alive: 2h\n",
			wantProblem:  "models.resident entry 1 has no id",
			wantResident: 0,
		},
		{
			name:         "the same id twice",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 2h\n    - id: m\n      keep_alive: -1\n",
			wantProblem:  "models.resident lists m more than once; the first entry stands",
			wantResident: 1,
		},
		{
			name:         "keep_alive zero would unload after every call",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 0\n",
			wantProblem:  "unloads the model after every call",
			wantResident: 0, // nothing else stands, so the entry is dropped
		},
		{
			name:         "keep_alive zero beside a window keeps the window",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 0\n      working_context: 8192\n",
			wantProblem:  "unloads the model after every call",
			wantResident: 1,
		},
		{
			name:         "keep_alive that is not a duration",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: soon\n      working_context: 8192\n",
			wantProblem:  `keep_alive "soon" is not a duration`,
			wantResident: 1,
		},
		{
			name:         "negative window",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 1h\n      working_context: -5\n",
			wantProblem:  "working_context -5 is outside 1 to 1048576",
			wantResident: 1, // the keep_alive still stands
		},
		{
			name:         "window past the cap",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      working_context: 99999999\n",
			wantProblem:  "working_context 99999999 is outside 1 to 1048576",
			wantResident: 0,
		},
		{
			name:         "entry that sets nothing",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n",
			wantProblem:  "models.resident m sets neither keep_alive nor working_context, so it does nothing",
			wantResident: 0,
		},
		{
			name:         "model the machine does not serve",
			doc:          "models:\n  allow: [other]\n  resident:\n    - id: m\n      keep_alive: -1\n",
			wantProblem:  "models.resident names m, which models.allow does not list",
			wantResident: 1, // reported, not dropped: allowing it later must just work
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := policyFromDoc(t, tt.doc)
			got := p.ModelsTuning()
			if got.Parallel != tt.wantParallel {
				t.Errorf("parallel = %d, want %d", got.Parallel, tt.wantParallel)
			}
			if len(got.Resident) != tt.wantResident {
				t.Errorf("resident = %+v, want %d entries", got.Resident, tt.wantResident)
			}
			var found bool
			for _, s := range p.ModelsTuningProblems() {
				if strings.Contains(s, tt.wantProblem) {
					found = true
				}
			}
			if !found {
				t.Errorf("no problem says %q; got %q", tt.wantProblem, p.ModelsTuningProblems())
			}
		})
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/tools/ -run 'TestModelsTuning' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/tools [build failed]
internal/worker/tools/models_tuning_policy_test.go:44:11: p.ModelsTuning undefined (type *Policy has no field or method ModelsTuning)
internal/worker/tools/models_tuning_policy_test.go:60:19: p.ModelsTuningProblems undefined (type *Policy has no field or method ModelsTuningProblems)
internal/worker/tools/models_tuning_policy_test.go:70:12: p.ModelsTuning undefined (type *Policy has no field or method ModelsTuning)
```

- [ ] **Step 3: Write the implementation**

Create `internal/worker/tools/models_tuning.go`:

```go
package tools

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// models.parallel and models.resident, read the way apps.levels is read:
// a pure function of the block as written, run on every read, that never
// fails. A shape it cannot use becomes a sentence about that entry and
// the entry is ignored -- the direction that changes nothing -- so a typo
// costs the one setting and never the machine.
//
// Design record: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

const (
	// MaxModelsParallel bounds models.parallel. Past it the number is a
	// typo, and a typo here is not harmless: the ceiling is also the sum
	// the engine rations by.
	MaxModelsParallel = 64
	// MaxWorkingContext bounds a resident model's working window, in
	// tokens. No local model serves more than a million.
	MaxWorkingContext = 1 << 20
)

// ModelsTuning is models.parallel and models.resident after validation.
type ModelsTuning struct {
	// Parallel is the owner's per-model ceiling for the native Ollama, or
	// zero when the file does not say (or said something unusable).
	Parallel int
	// Resident is every entry that stands, in file order. An entry for a
	// model the machine does not serve stands: allowing the model later
	// must just work, and ModelsTuningProblems says it is idle meanwhile.
	Resident []models.Resident
}

// ModelsTuning returns the validated tuning. The slice is the caller's
// own, built fresh on every call: a SIGHUP reload can land underneath a
// caller that is still deciding.
func (p *Policy) ModelsTuning() ModelsTuning {
	if p == nil {
		return ModelsTuning{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, _ := readModelsTuning(p.models)
	return t
}

// ModelsTuningProblems is every problem with models.parallel and
// models.resident, as sentences an owner can act on. The worker logs them
// when the file is read and `memql worker models` prints them, because a
// setting that silently did nothing is indistinguishable from one that
// worked.
func (p *Policy) ModelsTuningProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, problems := readModelsTuning(p.models)
	return problems
}

func readModelsTuning(m ModelsPolicy) (ModelsTuning, []string) {
	var (
		out      ModelsTuning
		problems []string
	)
	switch {
	case m.Parallel == 0:
		// Not said.
	case m.Parallel < 1 || m.Parallel > MaxModelsParallel:
		problems = append(problems, fmt.Sprintf(
			"models.parallel: %d is outside 1 to %d and is ignored; OLLAMA_NUM_PARALLEL, the managed Ollama unit, or 1 applies",
			m.Parallel, MaxModelsParallel))
	default:
		out.Parallel = m.Parallel
	}

	allowed := make(map[string]bool, len(m.Allow))
	for _, id := range m.Allow {
		allowed[strings.TrimSpace(id)] = true
	}

	seen := map[string]bool{}
	for i, raw := range m.Resident {
		n := i + 1
		id := strings.TrimSpace(raw.ID)
		if id == "" {
			problems = append(problems, fmt.Sprintf("models.resident entry %d has no id and is ignored", n))
			continue
		}
		if seen[id] {
			problems = append(problems, fmt.Sprintf("models.resident lists %s more than once; the first entry stands", id))
			continue
		}
		seen[id] = true

		r := models.Resident{ID: id}
		ka, err := models.ParseKeepAlive(raw.KeepAlive)
		if err != nil {
			problems = append(problems, fmt.Sprintf("models.resident %s: %v; keep_alive is ignored", id, err))
		} else {
			r.KeepAlive = ka
		}
		switch {
		case raw.WorkingContext == 0:
			// Not said.
		case raw.WorkingContext < 1 || raw.WorkingContext > MaxWorkingContext:
			problems = append(problems, fmt.Sprintf(
				"models.resident %s: working_context %d is outside 1 to %d and is ignored",
				id, raw.WorkingContext, MaxWorkingContext))
		default:
			r.WorkingContext = raw.WorkingContext
		}
		if !r.KeepAlive.Set && r.WorkingContext == 0 {
			// Nothing stands. Say so only when the entry never tried to say
			// anything: an entry whose fields were refused already has its
			// sentences.
			if strings.TrimSpace(raw.KeepAlive) == "" && raw.WorkingContext == 0 {
				problems = append(problems, fmt.Sprintf(
					"models.resident %s sets neither keep_alive nor working_context, so it does nothing", id))
			}
			continue
		}
		if !allowed[id] {
			problems = append(problems, fmt.Sprintf(
				"models.resident names %s, which models.allow does not list; this machine will not serve it, so nothing keeps it loaded", id))
		}
		out.Resident = append(out.Resident, r)
	}
	return out, problems
}
```

Modify `internal/worker/tools/policy.go`:

```diff
--- a/internal/worker/tools/policy.go
+++ b/internal/worker/tools/policy.go
@@ -212,6 +212,15 @@
 	// means "the file has no opinion", and the accessor answers that
 	// with the default. Do not "simplify" this to a bool.
 	Pull *bool `yaml:"pull"`
+	// Parallel is the per-model concurrency this machine advertises for its
+	// native Ollama (the `max=` on each model label), for a runtime whose
+	// OLLAMA_NUM_PARALLEL the cockpit cannot read from a unit it wrote. Zero
+	// is "not said". Validated by readModelsTuning.
+	Parallel int `yaml:"parallel"`
+	// Resident lists the models this machine keeps loaded and how
+	// (docs/superpowers/specs/2026-10-18-fleet-tuning-design.md). Absent
+	// changes nothing: no keep_alive is sent and no window is pinned.
+	Resident []models.ResidentModel `yaml:"resident"`
 }
 
 // BackupPolicy controls which folders this machine will back up into the
@@ -508,6 +517,12 @@
 	// operator nor this code intended -- an endpoint moved to a new port
 	// would keep answering on the old one until a restart.
 	p.models.Runtimes = raw.Models.Runtimes
+	// parallel and resident REPLACE, for models.pull's reason and one of
+	// their own: each is a setting whose absence is the default, so a merge
+	// would keep a model loaded for hours on the strength of a line the owner
+	// already deleted, and a SIGHUP is how they take the deletion back.
+	p.models.Parallel = raw.Models.Parallel
+	p.models.Resident = raw.Models.Resident
 	// pipelines REPLACES, whole, for the same reason: allow is a consent, and
 	// a narrowed repos list that kept the repository it dropped would admit a
 	// step the file no longer names.
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/tools/ -run 'TestModelsTuning' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestModelsTuning_LoadsFromPolicyYAML (0.00s)
--- PASS: TestModelsTuning_AbsentIsTheDefault (0.00s)
--- PASS: TestModelsTuning_ReloadReplacesRatherThanMerges (0.00s)
--- PASS: TestModelsTuning_AnEntryThatCannotStandIsReportedAndIgnored (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/tools	0.005s
```

- [ ] **Step 5: Run the package, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/tools/
go test ./internal/worker/tools/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/tools	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/tools/models_tuning.go \
  internal/worker/tools/models_tuning_policy_test.go \
  internal/worker/tools/policy.go
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(policy): models.parallel and models.resident" \
  -m "Two policy keys that change nothing until an owner writes them. Both replace on reload so deleting the block and sending SIGHUP puts every model back on Ollama's defaults. Entries that cannot stand are reported as sentences and ignored." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Discovery applies the owner's tuning

**Files:**
- Modify: `internal/worker/models/discover.go`
- Modify: `internal/worker/models/models.go`
- Modify: `internal/worker/models/ollama.go`
- Modify: `internal/worker/models/resident.go` (append helpers)
- Modify: `internal/worker/models/discover_test.go` (one call renamed)
- Test: `internal/worker/models/resident_discovery_test.go`

**Interfaces:**
- Consumes: `models.Resident`, `models.KeepAlive` (Task 2).
- Produces: `models.Request.Parallel int`, `models.Request.Resident []Resident`; `models.Discoverer.ManagedParallel func() int` (a seam; nil means no managed unit); `(*Discoverer).ParallelSource(configured int) (int, string)` returning the ceiling and the source's name ("models.parallel in policy.yaml", "OLLAMA_NUM_PARALLEL in the worker's environment", "the memql-ollama.service unit", "the default"); `models.Info.KeepAlive KeepAlive`, `models.Info.WorkingContext int` (only an Ollama model carries them; the window is capped at the model's own context); `Inventory.ProbeNotes` sentences for an entry that is idle.

Decisions D3 and D5. `ollamaParallelism` becomes `effectiveParallelism`, which asks `ParallelSource`; the existing test that called it is renamed to match (its table is unchanged). Settings ride `Info`, not `Attributes`, so the label gains nothing.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/models/resident_discovery_test.go`:

```go
package models

import (
	"context"
	"strings"
	"testing"
	"time"
)

// residentFixture is an Ollama with a chat model and an embedder, which is
// the pair every recommended set is.
func residentFixture(t *testing.T) string {
	t.Helper()
	srv, _ := ollamaStub(t, []string{"chat:27b", "embed:0.6b"}, map[string]string{
		"chat:27b":   `{"capabilities":["completion","tools"],"model_info":{"qwen35.context_length":262144}}`,
		"embed:0.6b": `{"capabilities":["embedding"],"model_info":{"qwen3.context_length":32768}}`,
	})
	return srv.URL
}

func byID(inv Inventory) map[string]Info {
	out := map[string]Info{}
	for _, m := range inv.Models {
		out[m.ID] = m
	}
	return out
}

// TestProbeOllama_ResidentSettingsRideTheModelNotTheLabel. keep_alive and
// the working window are this machine's business with its own runtime: the
// engine parses the label and skips a key it does not know, so putting
// either on the label would be a setting the engine can neither read nor
// reject. They ride Info.
func TestProbeOllama_ResidentSettingsRideTheModelNotTheLabel(t *testing.T) {
	url := residentFixture(t)
	inv := discovererFor(url, nil).Probe(context.Background(), Request{
		Allow:    []string{"chat:27b", "embed:0.6b"},
		Parallel: 2,
		Resident: []Resident{
			{ID: "chat:27b", KeepAlive: KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384},
			{ID: "embed:0.6b", KeepAlive: KeepAlive{Set: true, Forever: true}},
		},
	})
	got := byID(inv)

	chat := got["chat:27b"]
	if !chat.KeepAlive.Set || chat.KeepAlive.For != 2*time.Hour || chat.WorkingContext != 16384 {
		t.Errorf("chat model resident settings = %+v / %d", chat.KeepAlive, chat.WorkingContext)
	}
	embed := got["embed:0.6b"]
	if !embed.KeepAlive.Forever || embed.WorkingContext != 0 {
		t.Errorf("embedder resident settings = %+v / %d", embed.KeepAlive, embed.WorkingContext)
	}

	// Nothing about residency reaches the wire contract. The only label
	// change is the ceiling, which is the point of models.parallel.
	labels := inv.Labels()
	if want := "ctx=262144,structured=1,max=2,tools=1"; labels[Label("chat:27b")] != want {
		t.Errorf("chat label = %q, want %q", labels[Label("chat:27b")], want)
	}
	if want := "ctx=32768,embeddings=1,max=2"; labels[Label("embed:0.6b")] != want {
		t.Errorf("embedder label = %q, want %q", labels[Label("embed:0.6b")], want)
	}
}

// TestProbeOllama_NoResidentEntryChangesNothing. A machine whose owner
// wrote nothing sends no keep_alive and pins no window, so shipping the
// feature changes no machine until its owner opts in.
func TestProbeOllama_NoResidentEntryChangesNothing(t *testing.T) {
	url := residentFixture(t)
	inv := discovererFor(url, nil).Probe(context.Background(), Request{Allow: []string{"chat:27b", "embed:0.6b"}})
	for id, m := range byID(inv) {
		if m.KeepAlive.Set || m.WorkingContext != 0 {
			t.Errorf("%s carries resident settings with none configured: %+v / %d", id, m.KeepAlive, m.WorkingContext)
		}
		if m.MaxConcurrent != 1 {
			t.Errorf("%s max = %d, want 1", id, m.MaxConcurrent)
		}
	}
	if len(inv.ProbeNotes) != 0 {
		t.Errorf("notes with nothing configured: %v", inv.ProbeNotes)
	}
}

// TestProbeOllama_TheWindowNeverExceedsWhatTheModelCanHold. A window past
// the model's own context would be sent to the runtime as a request it
// cannot honour; the model's ceiling wins.
func TestProbeOllama_TheWindowNeverExceedsWhatTheModelCanHold(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"small:1b"}, map[string]string{
		"small:1b": `{"capabilities":["completion"],"model_info":{"llama.context_length":4096}}`,
	})
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"small:1b"},
		Resident: []Resident{{ID: "small:1b", WorkingContext: 16384}},
	})
	if got := byID(inv)["small:1b"].WorkingContext; got != 4096 {
		t.Errorf("working context = %d, want the model's own 4096", got)
	}
}

// TestProbeOllama_ResidentNotesNameWhatCannotWork. Each note is a reason
// an entry is idle, in the channel `memql worker models` already prints.
func TestProbeOllama_ResidentNotesNameWhatCannotWork(t *testing.T) {
	url := residentFixture(t)
	oa := openAIStub(t, []string{"remote:7b"})
	inv := discovererFor(url, nil).Probe(context.Background(), Request{
		Allow: []string{"chat:27b", "embed:0.6b", "remote:7b"},
		Runtimes: []DeclaredRuntime{{
			Name: "lmstudio", BaseURL: oa.URL,
			Models: []DeclaredModel{{ID: "remote:7b", StructuredOutput: true}},
		}},
		Resident: []Resident{
			{ID: "embed:0.6b", KeepAlive: KeepAlive{Set: true, Forever: true}, WorkingContext: 8192},
			{ID: "remote:7b", KeepAlive: KeepAlive{Set: true, Forever: true}},
			{ID: "ghost:70b", KeepAlive: KeepAlive{Set: true, Forever: true}},
		},
	})
	notes := strings.Join(inv.ProbeNotes, "\n")
	for _, want := range []string{
		"embed:0.6b is an embedding model; embeddings run at a fixed window, so working_context is ignored",
		"remote:7b is served by lmstudio, which has no keep_alive; models.resident is ignored for it",
		"ghost:70b is not installed in any runtime this machine probes",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note says %q:\n%s", want, notes)
		}
	}
	// A declared runtime's model never carries an Ollama-only setting.
	if m := byID(inv)["remote:7b"]; m.KeepAlive.Set || m.WorkingContext != 0 {
		t.Errorf("an OpenAI-compatible model carries resident settings: %+v / %d", m.KeepAlive, m.WorkingContext)
	}
}

// TestEffectiveParallelism. Whatever the sources say, the answer is never
// zero: the engine reads a missing max as unlimited.
func TestEffectiveParallelism(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		env        string
		managed    int
		want       int
	}{
		{"the owner's policy wins", 2, "4", 3, 2},
		{"the worker's environment beats the unit", 0, "4", 3, 4},
		{"the unit the cockpit wrote", 0, "", 3, 3},
		{"nothing said", 0, "", 0, 1},
		{"a garbage environment is not a source", 0, "nonsense", 0, 1},
		{"a garbage environment falls through to the unit", 0, "-3", 5, 5},
		{"a negative policy value is not a source", -1, "", 0, 1},
		{"a negative unit value is not a source", 0, "", -2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Discoverer{
				Getenv: func(k string) string {
					if k == "OLLAMA_NUM_PARALLEL" {
						return tt.env
					}
					return ""
				},
				ManagedParallel: func() int { return tt.managed },
			}
			got := d.effectiveParallelism(tt.configured)
			if got != tt.want {
				t.Errorf("effectiveParallelism(%d) env=%q unit=%d -> %d, want %d", tt.configured, tt.env, tt.managed, got, tt.want)
			}
			if got < 1 {
				t.Errorf("the ceiling must never be below 1, got %d", got)
			}
		})
	}
	// No seam at all is the common case: a Docker runtime, or a Mac.
	var d Discoverer
	if got := d.effectiveParallelism(0); got != 1 {
		t.Errorf("a discoverer with no sources -> %d, want 1", got)
	}
}

// TestParallelSource names where the ceiling came from, so the report can
// say it: a number whose origin is not stated is a number nobody can
// change.
func TestParallelSource(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		env        string
		managed    int
		want       int
		source     string
	}{
		{"policy", 2, "4", 3, 2, "models.parallel in policy.yaml"},
		{"environment", 0, "4", 3, 4, "OLLAMA_NUM_PARALLEL in the worker's environment"},
		{"unit", 0, "", 3, 3, "the memql-ollama.service unit"},
		{"default", 0, "", 0, 1, "the default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Discoverer{
				Getenv: func(k string) string {
					if k == "OLLAMA_NUM_PARALLEL" {
						return tt.env
					}
					return ""
				},
				ManagedParallel: func() int { return tt.managed },
			}
			n, source := d.ParallelSource(tt.configured)
			if n != tt.want || source != tt.source {
				t.Errorf("ParallelSource = %d from %q, want %d from %q", n, source, tt.want, tt.source)
			}
			if n != d.effectiveParallelism(tt.configured) {
				t.Errorf("ParallelSource and effectiveParallelism disagree")
			}
		})
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run 'TestProbeOllama_Resident|TestProbeOllama_NoResident|TestProbeOllama_TheWindow|TestEffectiveParallelism|TestParallelSource|TestOllamaParallelism' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/models [build failed]
internal/worker/models/resident_discovery_test.go:38:3: unknown field Parallel in struct literal of type Request
internal/worker/models/resident_discovery_test.go:39:3: unknown field Resident in struct literal of type Request
internal/worker/models/resident_discovery_test.go:47:11: chat.KeepAlive undefined (type Info has no field or method KeepAlive)
```

- [ ] **Step 3: Write the implementation**

Modify `internal/worker/models/resident.go`:

```diff
--- a/internal/worker/models/resident.go
+++ b/internal/worker/models/resident.go
@@ -146,3 +146,74 @@
 	// the engine's own window alone.
 	WorkingContext int
 }
+
+// residentByID indexes the validated entries. The first entry for an id
+// wins, matching the policy reader, so a hand-built Request cannot make a
+// later line quietly override an earlier one.
+func residentByID(list []Resident) map[string]Resident {
+	out := make(map[string]Resident, len(list))
+	for _, r := range list {
+		id := strings.TrimSpace(r.ID)
+		if id == "" {
+			continue
+		}
+		if _, dup := out[id]; !dup {
+			out[id] = r
+		}
+	}
+	return out
+}
+
+// applyResident puts an owner's entry on the model it names. Only an
+// Ollama model takes one: an OpenAI-compatible runtime has no keep_alive
+// to send, and sending its model an Ollama-shaped field would be a request
+// the runtime may reject.
+//
+// The window is capped at the context the model itself reported. Asking a
+// runtime for more than a model can hold is a request it cannot honour,
+// and the model's own ceiling is the number that was always going to win.
+func applyResident(info *Info, r Resident) {
+	if info.Kind != KindOllama {
+		return
+	}
+	if !r.KeepAlive.Set && r.WorkingContext <= 0 {
+		return
+	}
+	info.KeepAlive = r.KeepAlive
+	w := r.WorkingContext
+	if w > 0 && info.ContextWindow > 0 && w > info.ContextWindow {
+		w = info.ContextWindow
+	}
+	info.WorkingContext = w
+}
+
+// residentNotes says why an entry is idle, in the channel `memql worker
+// models` already prints (Inventory.ProbeNotes). An entry that names a
+// model the machine does not have is the likeliest mistake -- the bake-off
+// names a winner before anyone has pulled it -- and silence would read as
+// a model that is resident.
+func residentNotes(found []Info, resident []Resident) []string {
+	byModel := make(map[string]Info, len(found))
+	for _, m := range found {
+		byModel[m.ID] = m
+	}
+	var notes []string
+	seen := map[string]bool{}
+	for _, r := range resident {
+		id := strings.TrimSpace(r.ID)
+		if id == "" || seen[id] {
+			continue
+		}
+		seen[id] = true
+		m, ok := byModel[id]
+		switch {
+		case !ok:
+			notes = append(notes, fmt.Sprintf("models.resident: %s is not installed in any runtime this machine probes", id))
+		case m.Kind != KindOllama:
+			notes = append(notes, fmt.Sprintf("models.resident: %s is served by %s, which has no keep_alive; models.resident is ignored for it", id, m.Runtime))
+		case m.Embeddings && r.WorkingContext > 0:
+			notes = append(notes, fmt.Sprintf("models.resident: %s is an embedding model; embeddings run at a fixed window, so working_context is ignored", id))
+		}
+	}
+	return notes
+}
```

Modify `internal/worker/models/discover.go`:

```diff
--- a/internal/worker/models/discover.go
+++ b/internal/worker/models/discover.go
@@ -103,6 +103,11 @@
 	Allow []string
 	// Runtimes are the declared OpenAI-compatible endpoints.
 	Runtimes []DeclaredRuntime
+	// Parallel is policy.yaml models.parallel, validated. Zero is "not said".
+	Parallel int
+	// Resident is policy.yaml models.resident, validated. Empty changes
+	// nothing about how any model is called.
+	Resident []Resident
 }
 
 // Discoverer probes this machine. Every external fact is a seam, so the
@@ -119,6 +124,13 @@
 	OllamaBaseURL string
 	// FloorFn defaults to Floor.
 	FloorFn func() FloorVerdict
+	// ManagedParallel reports the OLLAMA_NUM_PARALLEL of a runtime this
+	// cockpit manages -- the unit it wrote, read where it lives -- or zero
+	// when there is none or it says nothing. A seam rather than a file read
+	// here, because this package knows nothing about systemd and the
+	// package that wrote the unit is the one that can read it back. Nil
+	// means there is no such runtime (Docker, a Mac, one started by hand).
+	ManagedParallel func() int
 }
 
 // DefaultOllamaBaseURL is where Ollama listens unless told otherwise.
@@ -177,7 +189,7 @@
 		}
 	}
 
-	found, note := d.probeOllama(ctx)
+	found, note := d.probeOllama(ctx, req)
 	if note != "" {
 		inv.ProbeNotes = append(inv.ProbeNotes, note)
 	}
@@ -214,6 +226,7 @@
 		return inv.Models[i].Runtime < inv.Models[j].Runtime
 	})
 	inv.Models, inv.ProbeNotes = resolveDuplicates(inv.Models, inv.ProbeNotes)
+	inv.ProbeNotes = append(inv.ProbeNotes, residentNotes(inv.Models, req.Resident)...)
 	return inv
 }
 
@@ -336,20 +349,54 @@
 	return strings.TrimRight(host, "/")
 }
 
-// ollamaParallelism is the per-model concurrency this machine will claim.
+// envParallelism is OLLAMA_NUM_PARALLEL as the worker's own environment
+// states it, or zero when it is unset or not a positive number.
+func (d *Discoverer) envParallelism() int {
+	if v := strings.TrimSpace(d.getenv("OLLAMA_NUM_PARALLEL")); v != "" {
+		if n, err := strconv.Atoi(v); err == nil && n > 0 {
+			return n
+		}
+	}
+	return 0
+}
+
+// effectiveParallelism is the per-model concurrency this machine will
+// claim. See ParallelSource for the sources and their order.
+func (d *Discoverer) effectiveParallelism(configured int) int {
+	n, _ := d.ParallelSource(configured)
+	return n
+}
+
+// ParallelSource is the per-model concurrency this machine will claim and
+// the sentence-ready name of where it came from. The sources are read most
+// deliberate first: the owner's models.parallel, the worker's
+// OLLAMA_NUM_PARALLEL (the only knob there was before the policy key, so it
+// keeps its place ahead of the unit), the unit this cockpit wrote for its
+// own Ollama, and one.
 //
 // ZERO IS NOT AN OPTION HERE, and this is the one place in the package
 // where absent is the PERMISSIVE direction rather than the safe one: the
 // engine reads a missing `max=` as unlimited. Left unset, a laptop with a
 // single 8B model would be handed as many concurrent generations as the
-// router had work for. So a ceiling is always declared -- OLLAMA_NUM_PARALLEL
-// when the operator set one, and otherwise one, which is what Ollama itself
-// falls back to when it cannot fit more.
-func (d *Discoverer) ollamaParallelism() int {
-	if v := strings.TrimSpace(d.getenv("OLLAMA_NUM_PARALLEL")); v != "" {
-		if n, err := strconv.Atoi(v); err == nil && n > 0 {
-			return n
-		}
-	}
-	return 1
-}
+// router had work for. So a ceiling is always declared, and a source that
+// holds a nonsense value is skipped rather than believed.
+//
+// The sources can disagree with the Ollama that is actually running --
+// the unit is edited and Ollama has not restarted yet -- and that is the
+// benign direction either way: a call admitted past the runtime's slots
+// queues inside Ollama, and a ceiling below them wastes a slot until the
+// next refresh.
+func (d *Discoverer) ParallelSource(configured int) (int, string) {
+	if configured > 0 {
+		return configured, "models.parallel in policy.yaml"
+	}
+	if n := d.envParallelism(); n > 0 {
+		return n, "OLLAMA_NUM_PARALLEL in the worker's environment"
+	}
+	if d != nil && d.ManagedParallel != nil {
+		if n := d.ManagedParallel(); n > 0 {
+			return n, "the memql-ollama.service unit"
+		}
+	}
+	return 1, "the default"
+}
```

Modify `internal/worker/models/models.go`:

```diff
--- a/internal/worker/models/models.go
+++ b/internal/worker/models/models.go
@@ -371,6 +371,19 @@
 	// Allowed mirrors policy.yaml models.allow.
 	Allowed bool
 
+	// KeepAlive and WorkingContext are this machine's instructions to its
+	// own Ollama for THIS model (policy.yaml models.resident). They are not
+	// Attributes, and the difference is the point: Attributes are what the
+	// engine is told, and the engine has no use for either of these. A key
+	// on the label that the engine does not parse is skipped without a
+	// word, which is a setting nobody can see fail.
+	//
+	// Both are the zero value unless the owner wrote an entry, and only an
+	// Ollama model ever carries them -- an OpenAI-compatible runtime has no
+	// keep_alive to send it.
+	KeepAlive      KeepAlive
+	WorkingContext int
+
 	Attributes
 }
 
```

Modify `internal/worker/models/ollama.go`:

```diff
--- a/internal/worker/models/ollama.go
+++ b/internal/worker/models/ollama.go
@@ -55,7 +55,7 @@
 // ordinary case across most of a fleet, and it means "this machine offers
 // no models" -- a fact the worker reports, not a condition it logs at warn
 // level on every registration.
-func (d *Discoverer) probeOllama(ctx context.Context) ([]Info, string) {
+func (d *Discoverer) probeOllama(ctx context.Context, req Request) ([]Info, string) {
 	base := d.ollamaBaseURL()
 
 	var tags ollamaTagsResponse
@@ -66,7 +66,8 @@
 		return nil, fmt.Sprintf("Ollama is running at %s but has no models pulled", base)
 	}
 
-	parallel := d.ollamaParallelism()
+	parallel := d.effectiveParallelism(req.Parallel)
+	resident := residentByID(req.Resident)
 	out := make([]Info, 0, len(tags.Models))
 	for _, m := range tags.Models {
 		id := strings.TrimSpace(m.Model)
@@ -134,6 +135,9 @@
 			// declares it under models.runtimes; a bare Ollama offers
 			// neither, which is true.
 		}
+		// AFTER the probe, because the window is capped by the context the
+		// probe read. Settings ride the model, never the label.
+		applyResident(&info, resident[id])
 		out = append(out, info)
 	}
 	return out, ""
```

Modify `internal/worker/models/discover_test.go`:

```diff
--- a/internal/worker/models/discover_test.go
+++ b/internal/worker/models/discover_test.go
@@ -195,7 +195,7 @@
 			}
 			return ""
 		}}
-		if got := d.ollamaParallelism(); got != want {
+		if got := d.effectiveParallelism(0); got != want {
 			t.Errorf("OLLAMA_NUM_PARALLEL=%q -> %d, want %d", env, got, want)
 		}
 	}
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run 'TestProbeOllama_Resident|TestProbeOllama_NoResident|TestProbeOllama_TheWindow|TestEffectiveParallelism|TestParallelSource|TestOllamaParallelism' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestOllamaParallelism_NeverZero (0.00s)
--- PASS: TestProbeOllama_ResidentSettingsRideTheModelNotTheLabel (0.00s)
--- PASS: TestProbeOllama_NoResidentEntryChangesNothing (0.00s)
--- PASS: TestProbeOllama_TheWindowNeverExceedsWhatTheModelCanHold (0.00s)
--- PASS: TestProbeOllama_ResidentNotesNameWhatCannotWork (0.00s)
--- PASS: TestEffectiveParallelism (0.00s)
--- PASS: TestParallelSource (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	0.007s
```

- [ ] **Step 5: Run the package, format and vet (this also runs `TestWireContract`, which must stay green)**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/models/
go test ./internal/worker/models/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/models/
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(models): discovery applies the owner's residency and slot settings" \
  -m "The per-model ceiling now has named sources in a fixed order (policy, the worker's environment, the managed unit, one) and is never zero. Resident settings ride models.Info and never the label, so the engine hears nothing new. Entries that cannot work are explained in the probe notes." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: One place builds every discovery

**Files:**
- Modify: `internal/worker/modelinventory.go`
- Modify: `internal/worker/cli.go`
- Modify: `internal/worker/pair.go`
- Modify: `internal/worker/probe_cmd.go`
- Modify: `internal/worker/runtime_cmd.go`
- Modify: `internal/worker/inference_cmd.go`
- Modify: `internal/worker/models_cmd.go`
- Test: `internal/worker/models_tuning_wiring_test.go`

**Interfaces:**
- Consumes: `tools.Policy.ModelsTuning()` and `ModelsTuningProblems()` (Task 3); `models.Request.Parallel/Resident` (Task 4).
- Produces: `newDiscoverer() *models.Discoverer` and `modelsRequest(*tools.Policy) models.Request` in package `worker`; the worker logs `policy.yaml models has a problem` at start and at SIGHUP.

Seven places built a `models.Discoverer` and three built the request from the policy by hand. `memql worker models` promises that what it prints is what the cluster would be told, so a command that built its own would stop hearing about the tuning and print a ceiling the worker does not advertise, silently. An AST walk (the style of `TestEveryRunnerConstructionWiresTheLivePolicy`) holds the seven to one constructor.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/models_tuning_wiring_test.go`:

```go
package worker

import (
	"bytes"
	"go/ast"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func tuningPolicy(t *testing.T, doc string) *tools.Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p
}

// TestModelsRequestCarriesThePolicysTuning. The request is what every
// discovery in this package is asked, so what the policy says about
// residency and concurrency has to be in it.
func TestModelsRequestCarriesThePolicysTuning(t *testing.T) {
	p := tuningPolicy(t, `models:
  allow:
    - qwen3.8:27b
  parallel: 2
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
`)
	req := modelsRequest(p)
	if len(req.Allow) != 1 || req.Allow[0] != "qwen3.8:27b" {
		t.Errorf("allow = %v", req.Allow)
	}
	if req.Parallel != 2 {
		t.Errorf("parallel = %d, want 2", req.Parallel)
	}
	want := models.Resident{ID: "qwen3.8:27b", KeepAlive: models.KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384}
	if len(req.Resident) != 1 || req.Resident[0] != want {
		t.Errorf("resident = %+v, want [%+v]", req.Resident, want)
	}
}

// TestEveryDiscoveryIsBuiltInOnePlace. Seven commands and run paths probe
// this machine, and `memql worker models` promises that what it prints is
// what the cluster would be told. If one of them builds its own
// Discoverer or its own Request it stops hearing about the tuning, and the
// disagreement is silent: the worker advertises one ceiling while the
// command that exists to explain it prints another.
func TestEveryDiscoveryIsBuiltInOnePlace(t *testing.T) {
	fset, files := workerSources(t)
	var discovererLits, requestLits []string
	callers := map[string]int{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "models" {
					return true
				}
				pos := fset.Position(x.Pos())
				where := pos.Filename + ":" + strconv.Itoa(pos.Line)
				switch sel.Sel.Name {
				case "Discoverer":
					discovererLits = append(discovererLits, where)
				case "Request":
					requestLits = append(requestLits, where)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "newDiscoverer" {
					callers[path]++
				}
			}
			return true
		})
	}
	if len(discovererLits) != 1 || !strings.HasPrefix(discovererLits[0], "modelinventory.go:") {
		t.Errorf("models.Discoverer literals at %v; want exactly one, inside newDiscoverer in modelinventory.go", discovererLits)
	}
	if len(requestLits) != 1 || !strings.HasPrefix(requestLits[0], "modelinventory.go:") {
		t.Errorf("models.Request literals at %v; want exactly one, inside modelsRequest in modelinventory.go", requestLits)
	}
	// The scan reports its own coverage, for the reason the Runner scan
	// does: a walk that found nothing would pass every assertion above.
	var missing []string
	for _, site := range []string{"cli.go", "pair.go", "models_cmd.go", "inference_cmd.go", "probe_cmd.go", "runtime_cmd.go", "modelinventory.go"} {
		if callers[site] == 0 {
			missing = append(missing, site)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("no newDiscoverer call in %v (found %v): the scan is not looking where discoverers are built", missing, callers)
	}
}

// TestPolicyProblemsAreLoggedAtLoadAndReload. A setting that silently did
// nothing is indistinguishable from one that worked, so the worker says so
// at the two moments the owner could have changed it: start and SIGHUP.
func TestPolicyProblemsAreLoggedAtLoadAndReload(t *testing.T) {
	p := tuningPolicy(t, "models:\n  parallel: 500\n  resident:\n    - id: m\n      keep_alive: 0\n")
	var buf bytes.Buffer
	logLevelProblems(slog.New(slog.NewJSONHandler(&buf, nil)), p)
	out := buf.String()
	for _, want := range []string{
		"policy.yaml models has a problem",
		"models.parallel: 500 is outside 1 to 64",
		"unloads the model after every call",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not say %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/ -run 'TestModelsRequest|TestEveryDiscovery|TestPolicyProblems' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker [build failed]
internal/worker/models_tuning_wiring_test.go:45:9: undefined: modelsRequest
```

- [ ] **Step 3: Write the implementation (two imports of `models` become unused in `cli.go` and `pair.go`; the diffs remove them)**

Modify `internal/worker/modelinventory.go`:

```diff
--- a/internal/worker/modelinventory.go
+++ b/internal/worker/modelinventory.go
@@ -71,6 +71,30 @@
 	now    func() time.Time
 }
 
+// newDiscoverer is the ONE place a models.Discoverer is built. Seven
+// commands and run paths probe this machine, and every one of them has to
+// read the same sources for the same facts -- the worker's environment, the
+// unit it manages -- or `memql worker models` prints a ceiling the worker
+// does not advertise. TestEveryDiscoveryIsBuiltInOnePlace holds the seven
+// to it.
+func newDiscoverer() *models.Discoverer {
+	return &models.Discoverer{}
+}
+
+// modelsRequest is what this machine's policy asks discovery: which models
+// may be offered, which runtimes are declared, and the owner's tuning --
+// the per-model ceiling and the models kept loaded. The ONE place that
+// reads those out of the policy, for newDiscoverer's reason.
+func modelsRequest(policy *tools.Policy) models.Request {
+	tuning := policy.ModelsTuning()
+	return models.Request{
+		Allow:    policy.ModelsAllow(),
+		Runtimes: policy.ModelRuntimes(),
+		Parallel: tuning.Parallel,
+		Resident: tuning.Resident,
+	}
+}
+
 // NewModelInventory builds the reporter the worker runs with.
 //
 // The discoverer is the CALLER's, not built here, because the runner's
@@ -78,7 +102,7 @@
 // one would read OLLAMA_HOST again, and two readings drift.
 func NewModelInventory(policy *tools.Policy, discoverer *models.Discoverer) ModelInventory {
 	if discoverer == nil {
-		discoverer = &models.Discoverer{}
+		discoverer = newDiscoverer()
 	}
 	return &policyModelInventory{
 		discoverer: discoverer,
@@ -103,10 +127,7 @@
 	if !p.at.IsZero() && p.clock().Sub(p.at) < p.ttl {
 		return p.cached
 	}
-	p.cached = p.discoverer.Discover(ctx, models.Request{
-		Allow:    p.policy.ModelsAllow(),
-		Runtimes: p.policy.ModelRuntimes(),
-	})
+	p.cached = p.discoverer.Discover(ctx, modelsRequest(p.policy))
 	p.at = p.clock()
 	return p.cached
 }
```

Modify `internal/worker/cli.go`:

```diff
--- a/internal/worker/cli.go
+++ b/internal/worker/cli.go
@@ -23,7 +23,6 @@
 	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
-	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
 )
 
@@ -369,7 +368,7 @@
 	// directory an app installer uses (servicepath.go).
 	ensureServicePath(logger)
 
-	discoverer := &models.Discoverer{}
+	discoverer := newDiscoverer()
 	modelInventory := NewModelInventory(policy, discoverer)
 	appInventories := NewAppInventories(policy, nil)
 
@@ -1070,6 +1069,9 @@
 	for _, problem := range policy.AppLevelProblems() {
 		logger.Warn("policy.yaml apps.levels has a problem", "problem", problem)
 	}
+	for _, problem := range policy.ModelsTuningProblems() {
+		logger.Warn("policy.yaml models has a problem", "problem", problem)
+	}
 }
 
 // reloadPolicy re-reads policy.yaml on SIGHUP and says what changed that the
```

Modify `internal/worker/pair.go`:

```diff
--- a/internal/worker/pair.go
+++ b/internal/worker/pair.go
@@ -14,7 +14,6 @@
 	"time"
 
 	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
-	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
 	"github.com/znasllc-io/memql/component/identity/workerpairing"
 )
@@ -232,7 +231,7 @@
 	// As `worker run` does, before anything resolves a binary.
 	ensureServicePath(logger)
 
-	discoverer := &models.Discoverer{}
+	discoverer := newDiscoverer()
 	modelInventory := NewModelInventory(policy, discoverer)
 	appInventories := NewAppInventories(policy, nil)
 
```

Modify `internal/worker/probe_cmd.go`:

```diff
--- a/internal/worker/probe_cmd.go
+++ b/internal/worker/probe_cmd.go
@@ -56,7 +56,7 @@
 }
 
 func newProbeRun(configPath, modelID string, suite int) *probeRun {
-	discoverer := &models.Discoverer{}
+	discoverer := newDiscoverer()
 	policyPath := filepath.Join(filepath.Dir(configPath), "policy.yaml")
 	return &probeRun{
 		out:        os.Stdout,
```

Modify `internal/worker/runtime_cmd.go`:

```diff
--- a/internal/worker/runtime_cmd.go
+++ b/internal/worker/runtime_cmd.go
@@ -42,7 +42,7 @@
 
 func newRuntimeSetup(configPath, name string, nonInteractive bool) *runtimeSetup {
 	policyPath := filepath.Join(filepath.Dir(configPath), "policy.yaml")
-	discoverer := &models.Discoverer{}
+	discoverer := newDiscoverer()
 	return &runtimeSetup{
 		out:            os.Stdout,
 		in:             os.Stdin,
```

Modify `internal/worker/inference_cmd.go`:

```diff
--- a/internal/worker/inference_cmd.go
+++ b/internal/worker/inference_cmd.go
@@ -139,7 +139,7 @@
 // this command just changed.
 func newInferenceSetup(configPath string, ids []string, nonInteractive bool, runtimeFlag string) *inferenceSetup {
 	policyPath := filepath.Join(filepath.Dir(configPath), "policy.yaml")
-	discoverer := &models.Discoverer{}
+	discoverer := newDiscoverer()
 	return &inferenceSetup{
 		out:            os.Stdout,
 		in:             os.Stdin,
@@ -186,7 +186,7 @@
 // line -- an id that Allow refused to write is then visibly absent.
 func probeWithPolicy(ctx context.Context, d *models.Discoverer, policyPath string) models.Inventory {
 	policy := policyAt(policyPath)
-	return d.Probe(ctx, models.Request{Allow: policy.ModelsAllow(), Runtimes: policy.ModelRuntimes()})
+	return d.Probe(ctx, modelsRequest(policy))
 }
 
 // runInferenceSetup runs the flow and returns the process exit code.
```

Modify `internal/worker/models_cmd.go`:

```diff
--- a/internal/worker/models_cmd.go
+++ b/internal/worker/models_cmd.go
@@ -74,11 +74,9 @@
 	// to see that they have three models pulled, because "you have models
 	// but this machine cannot serve them" and "you have no models" send
 	// them to entirely different places.
-	inv := (&models.Discoverer{}).Probe(ctx, models.Request{
-		Allow:    policy.ModelsAllow(),
-		Runtimes: policy.ModelRuntimes(),
-	})
+	inv := newDiscoverer().Probe(ctx, modelsRequest(policy))
 	printModelInventory(os.Stdout, inv, policyPath)
+
 }
 
 // printModelInventory writes the report. Split out so the shape is
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/ -run 'TestModelsRequest|TestEveryDiscovery|TestPolicyProblems' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestModelsRequestCarriesThePolicysTuning (0.00s)
--- PASS: TestEveryDiscoveryIsBuiltInOnePlace (0.01s)
--- PASS: TestPolicyProblemsAreLoggedAtLoadAndReload (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	0.023s
```

- [ ] **Step 5: Run the package, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/
go test ./internal/worker/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "refactor(worker): build every discoverer and discovery request in one place" \
  -m "newDiscoverer and modelsRequest are the only places a Discoverer or a Request is built, held by an AST walk, so a command cannot print a ceiling the worker does not advertise. Policy problems with the new keys are logged at start and at SIGHUP." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: The Ollama client sends keep_alive and pins the working window

**Files:**
- Modify: `internal/worker/modelcall/ollama.go`
- Modify: `internal/worker/modelcall/modalities.go` (GenerateImage)
- Modify: `internal/worker/modelcall/session.go` (NewClient)
- Test: `internal/worker/modelcall/ollama_resident_test.go`

**Interfaces:**
- Consumes: `models.Info.KeepAlive`, `models.Info.WorkingContext`, `models.KeepAlive.Wire()` (Tasks 2 and 4).
- Produces: `ollamaClient.keepAlive`, `ollamaClient.workingContext`; `(*ollamaClient).stampKeepAlive(body map[string]any)`; `(*ollamaClient).window(requested int64) int64`; `NewClient` copies both from the `Info`, so the serving path and `memql worker probe` use the same settings.

Decisions D1 and D2, and the two Review Focus lines that are about what is sent. The test file enumerates the four routes; a fifth added later has to be added to it. Measured against a real Ollama 0.34.2 (spec 3.3): with `working_context` 4096 and `keep_alive` 2h, calls whose engine windows were 2048, 4096, 1024, 4096, 8192, 2048 produced three loads, against six with nothing configured.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/modelcall/ollama_resident_test.go`:

```go
package modelcall

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// ollama_resident_test.go pins what this machine tells its own Ollama
// about keeping a model loaded.
//
// Ollama applies the keep_alive a request states, up or down, and what a
// request that omits it does depends on the release (0.34.2 leaves the
// expiry alone; a model whose FIRST request omitted it loads with Ollama's
// five minutes). One route that leaves the field out -- an embedding, a
// vision turn, an image -- can therefore start a model the owner asked to
// keep for two hours at five minutes, and nothing anywhere says so.

var (
	foreverKeepAlive = models.KeepAlive{Set: true, Forever: true}
	twoHourKeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
)

const (
	ollamaEmbedFrame = `{"model":"m","embeddings":[[0.1,0.2]],"prompt_eval_count":3}`
	ollamaImageFrame = `{"model":"m","images":["aGk="]}`
)

// sentKeepAlive decodes the keep_alive a request carried: nil when the
// key was never sent. The raw message is kept so the test can tell the
// JSON number -1 from the string "-1", which Ollama refuses.
func sentKeepAlive(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var sent struct {
		KeepAlive json.RawMessage `json:"keep_alive"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return sent.KeepAlive
}

func sentNumCtx(t *testing.T, body []byte) int64 {
	t.Helper()
	var sent struct {
		Options struct {
			NumCtx int64 `json:"num_ctx"`
		} `json:"options"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return sent.Options.NumCtx
}

// TestOllamaKeepAlive_EveryGenerationRouteCarriesIt. Chat, vision (a chat
// turn), embeddings and image generation are the four routes this client
// sends a model work on; a fifth added later has to be added here.
func TestOllamaKeepAlive_EveryGenerationRouteCarriesIt(t *testing.T) {
	routes := []struct {
		name  string
		reply string
		call  func(c *ollamaClient) error
	}{
		{"chat", ollamaDoneFrame, func(c *ollamaClient) error {
			_, err := c.Chat(context.Background(), ChatRequest{Model: "m"}, discardEmit)
			return err
		}},
		{"vision", ollamaDoneFrame, func(c *ollamaClient) error {
			_, err := c.Vision(context.Background(), VisionRequest{
				Model:    "m",
				Messages: []Message{{Role: "user", Content: "what is this"}},
				Images:   []ImagePart{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
			}, discardEmit)
			return err
		}},
		{"embedding", ollamaEmbedFrame, func(c *ollamaClient) error {
			_, err := c.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"hi"}})
			return err
		}},
		{"image", ollamaImageFrame, func(c *ollamaClient) error {
			_, err := c.GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "a cube"})
			return err
		}},
	}
	for _, ka := range []struct {
		name string
		set  models.KeepAlive
		want string
	}{
		// Forever is the JSON NUMBER -1. The string "-1" has no unit and
		// Ollama's duration parser refuses it.
		{"forever", foreverKeepAlive, `-1`},
		{"a duration", twoHourKeepAlive, `"2h0m0s"`},
	} {
		for _, r := range routes {
			t.Run(ka.name+"/"+r.name, func(t *testing.T) {
				srv, body := serveRaw(t, r.reply)
				c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), keepAlive: ka.set}
				if err := r.call(c); err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
				if got := string(sentKeepAlive(t, *body)); got != ka.want {
					t.Errorf("%s sent keep_alive %s, want %s", r.name, got, ka.want)
				}
			})
		}
	}
}

// TestOllamaKeepAlive_UnsetSendsNothing. A machine whose owner wrote no
// entry behaves exactly as it did before this feature: the key is absent
// and Ollama's own default stands.
func TestOllamaKeepAlive_UnsetSendsNothing(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: 8192}}, discardEmit); err != nil {
		t.Fatal(err)
	}
	if got := sentKeepAlive(t, *body); got != nil {
		t.Errorf("keep_alive = %s with nothing configured, want the key absent", got)
	}
	if got := sentNumCtx(t, *body); got != 8192 {
		t.Errorf("num_ctx = %d, want the engine's 8192 untouched", got)
	}
}

// TestOllamaWorkingContext. Ollama reloads a model whenever the requested
// num_ctx differs from the loaded one, and the engine sends the next power
// of two above each call: measured on this fleet, 107 of 170 reloads in
// thirty days were a bucket change, not an idle timeout. The window makes
// every call that fits run at the same num_ctx.
func TestOllamaWorkingContext(t *testing.T) {
	const window = 16384
	tests := []struct {
		name      string
		requested int64
		want      int64
	}{
		{"a small call runs at the window", 8192, window},
		{"a call at the window runs at it", window, window},
		// Review focus: a call that needs MORE than the window runs at
		// what it asked for. Clamping it down would truncate a prompt.
		{"a larger call is never clamped down", 32768, 32768},
		{"a much larger call is never clamped down", 131072, 131072},
		// The engine sent no window: the runtime default would apply, and
		// the owner asked for a stable one.
		{"no request runs at the window", 0, window},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, body := serveRaw(t, ollamaDoneFrame)
			c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), workingContext: window}
			if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: tt.requested}}, discardEmit); err != nil {
				t.Fatal(err)
			}
			if got := sentNumCtx(t, *body); got != tt.want {
				t.Errorf("num_ctx = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOllamaWorkingContext_EmbeddingsKeepTheirFixedWindow. An embedder
// runs at 8192 whatever the chat window is, because its cache would
// otherwise be sized for the chat model's (embedWorkingContext).
func TestOllamaWorkingContext_EmbeddingsKeepTheirFixedWindow(t *testing.T) {
	srv, body := serveRaw(t, ollamaEmbedFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), workingContext: 32768}
	if _, err := c.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"hi"}}); err != nil {
		t.Fatal(err)
	}
	if got := sentNumCtx(t, *body); got != embedWorkingContext {
		t.Errorf("embedding num_ctx = %d, want the fixed %d", got, embedWorkingContext)
	}
}

// TestNewClient_CarriesTheModelsResidentSettings. NewClient is the single
// place a runtime is reached from a models.Info, so it is where the
// settings discovery attached have to be picked up -- by the manager that
// serves calls and by the probe, which must measure the path serving uses.
func TestNewClient_CarriesTheModelsResidentSettings(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	info := models.Info{
		ID: "m", Kind: models.KindOllama, BaseURL: srv.URL,
		KeepAlive: twoHourKeepAlive, WorkingContext: 16384,
	}
	c := NewClient(info, srv.Client(), nil)
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: 4096}}, discardEmit); err != nil {
		t.Fatal(err)
	}
	if got := string(sentKeepAlive(t, *body)); got != `"2h0m0s"` {
		t.Errorf("keep_alive = %s, want the model's 2h0m0s", got)
	}
	if got := sentNumCtx(t, *body); got != 16384 {
		t.Errorf("num_ctx = %d, want the model's 16384", got)
	}
}

// TestOpenAICompatibleRuntimesAreNeverSentKeepAlive. keep_alive is an
// Ollama field; a declared runtime's model never carries one (discovery
// refuses to attach it), and the client must not invent one.
func TestOpenAICompatibleRuntimesAreNeverSentKeepAlive(t *testing.T) {
	srv, body := serveRaw(t, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	info := models.Info{
		ID: "m", Kind: models.KindOpenAICompatible, BaseURL: srv.URL,
		KeepAlive: foreverKeepAlive, WorkingContext: 16384, // as a bug would leave them
	}
	c := NewClient(info, srv.Client(), nil)
	_, _ = c.Chat(context.Background(), ChatRequest{Model: "m"}, discardEmit)
	if got := sentKeepAlive(t, *body); got != nil {
		t.Errorf("an OpenAI-compatible request carried keep_alive %s", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/modelcall/ -run 'TestOllamaKeepAlive|TestOllamaWorkingContext|TestNewClient_Carries|TestOpenAICompatibleRuntimesAreNever' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall [build failed]
internal/worker/modelcall/ollama_resident_test.go:102:62: unknown field keepAlive in struct literal of type ollamaClient
internal/worker/modelcall/ollama_resident_test.go:156:61: unknown field workingContext in struct literal of type ollamaClient
internal/worker/modelcall/ollama_resident_test.go:172:59: unknown field workingContext in struct literal of type ollamaClient
```

- [ ] **Step 3: Write the implementation**

Modify `internal/worker/modelcall/ollama.go`:

```diff
--- a/internal/worker/modelcall/ollama.go
+++ b/internal/worker/modelcall/ollama.go
@@ -8,6 +8,8 @@
 	"fmt"
 	"net/http"
 	"strings"
+
+	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
 )
 
 // The Ollama client. Chat streams newline-delimited JSON from /api/chat;
@@ -16,6 +18,48 @@
 type ollamaClient struct {
 	baseURL string
 	http    *http.Client
+
+	// keepAlive and workingContext are this model's residency settings
+	// (policy.yaml models.resident), put on the client by NewClient. The
+	// zero values send nothing and change nothing.
+	keepAlive      models.KeepAlive
+	workingContext int
+}
+
+// stampKeepAlive puts the model's keep_alive on a request body.
+//
+// EVERY ROUTE THAT SENDS THE MODEL WORK CALLS THIS. Ollama applies the
+// keep_alive a request STATES from that moment, in either direction -- a
+// request saying 90s shortens a residency of hours -- and what a request
+// that OMITS it does depends on the release. 0.34.2 leaves the expiry alone
+// (measured 2026-10-18), but a model whose first request omitted it loads
+// with the five-minute default, and an older release may fall back to the
+// default on every omission. A residency that depends on which route
+// touched the model first, or on the Ollama version, is not the one the
+// owner asked for. Chat (and vision, which is a chat turn), embeddings and
+// image generation are the routes today.
+func (c *ollamaClient) stampKeepAlive(body map[string]any) {
+	if wire := c.keepAlive.Wire(); wire != nil {
+		body["keep_alive"] = wire
+	}
+}
+
+// window is the num_ctx to send for a call that needs `requested` tokens
+// (zero when the engine named none).
+//
+// Ollama reloads a model whenever the requested num_ctx differs from the
+// one it loaded with, larger or smaller, even by a token. The engine rounds
+// each call up to a power of two (contextStartForCandidate in
+// integrations/agent/worker/fleet_context_allocation.go), so the same model
+// is asked for 8192, 16384 and 32768 in turn. With a working window set,
+// every call that fits in it runs at exactly the window and finds the
+// runner the last one left. A call that needs MORE runs at what it asked
+// for: clamping it down would truncate a prompt.
+func (c *ollamaClient) window(requested int64) int64 {
+	if w := int64(c.workingContext); w > 0 && requested <= w {
+		return w
+	}
+	return requested
 }
 
 // ollamaChatChunk is one NDJSON line from /api/chat. The same shape
@@ -62,9 +106,12 @@
 		"truncate": false,
 		"shift":    false,
 	}
-	if opts := ollamaOptions(req.Params); len(opts) > 0 {
+	params := req.Params
+	params.ContextTokens = c.window(params.ContextTokens)
+	if opts := ollamaOptions(params); len(opts) > 0 {
 		body["options"] = opts
 	}
+	c.stampKeepAlive(body)
 	if len(req.Schema) > 0 {
 		// Ollama takes the JSON Schema directly as `format`. It is
 		// passed through verbatim rather than re-marshalled: the engine
@@ -211,12 +258,14 @@
 	if req.Model == "qwen3-embedding:0.6b" {
 		options["num_batch"] = 512
 	}
-	resp, err := c.post(ctx, "/api/embed", map[string]any{
+	body := map[string]any{
 		"model":    req.Model,
 		"input":    req.Input,
 		"truncate": false,
 		"options":  options,
-	})
+	}
+	c.stampKeepAlive(body)
+	resp, err := c.post(ctx, "/api/embed", body)
 	if err != nil {
 		return Result{}, err
 	}
```

Modify `internal/worker/modelcall/modalities.go`:

```diff
--- a/internal/worker/modelcall/modalities.go
+++ b/internal/worker/modelcall/modalities.go
@@ -431,6 +431,7 @@
 	if opts := imageOptions(req); len(opts) > 0 {
 		request["options"] = opts
 	}
+	c.stampKeepAlive(request)
 	resp, err := c.post(ctx, "/api/generate", request)
 	if err != nil {
 		return ImageResult{}, err
```

Modify `internal/worker/modelcall/session.go`:

```diff
--- a/internal/worker/modelcall/session.go
+++ b/internal/worker/modelcall/session.go
@@ -864,7 +864,10 @@
 		}
 		return &openAIClient{baseURL: info.BaseURL, apiKey: key, http: httpClient, transcription: info.Transcription, voices: info.Voices}
 	}
-	return &ollamaClient{baseURL: info.BaseURL, http: httpClient}
+	return &ollamaClient{
+		baseURL: info.BaseURL, http: httpClient,
+		keepAlive: info.KeepAlive, workingContext: info.WorkingContext,
+	}
 }
 
 // -----------------------------------------------------------------------------
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/modelcall/ -run 'TestOllamaKeepAlive|TestOllamaWorkingContext|TestNewClient_Carries|TestOpenAICompatibleRuntimesAreNever' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestOllamaKeepAlive_EveryGenerationRouteCarriesIt (0.00s)
--- PASS: TestOllamaKeepAlive_UnsetSendsNothing (0.00s)
--- PASS: TestOllamaWorkingContext (0.00s)
--- PASS: TestOllamaWorkingContext_EmbeddingsKeepTheirFixedWindow (0.00s)
--- PASS: TestNewClient_CarriesTheModelsResidentSettings (0.00s)
--- PASS: TestOpenAICompatibleRuntimesAreNeverSentKeepAlive (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	0.011s
```

- [ ] **Step 5: Run the package, format and vet (modelcall takes about 30 s)**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/modelcall/
go test ./internal/worker/modelcall/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/modelcall/
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(modelcall): send keep_alive on every Ollama route and pin the working window" \
  -m "Ollama applies the keep_alive a request states, and what an omission does varies by release, so chat, vision, embeddings and image generation all state it. A working window floors num_ctx so the power-of-two windows the engine asks for stop reloading the model; a larger call still runs at what it asked for." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Say when a call waits for a concurrency slot

**Files:**
- Modify: `internal/worker/modelcall/session.go`
- Test: `internal/worker/modelcall/slotwait_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `modelcall.Options.SlotWaitLogFloor time.Duration` (zero means one second); info line `model call waited for a concurrency slot` and warn line `model call refused after waiting for a concurrency slot`, each with `request_id`, `model`, `call_level`, `purpose`, `waited_ms`, `model_max` (the refusal adds `error_code`).

Decision D6. Reserving slots for the fast level is deliberately not built: nothing on the wire says which calls are the pals' (spec 4, D6). This is the evidence that would justify it. Before this task a call that waited ninety seconds and was refused left a debug line, which the default log level never prints.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/modelcall/slotwait_test.go`:

```go
package modelcall

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// slotwait_test.go pins the one thing this machine says about contention.
//
// Whether a background call starves a fast one is a question about this
// machine's traffic, and the only answer anybody will have is the log:
// before this, a call that waited ninety seconds for a slot and was then
// refused left a debug line, which the default level never prints.

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAWaitedSlotIsLoggedWithItsLevelAndPurpose. The level and purpose are
// the only things on the wire that say which kind of work held the slot
// and which was kept waiting, so they are what the line carries.
func TestAWaitedSlotIsLoggedWithItsLevelAndPurpose(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: 20 * time.Millisecond,
	})

	holder := startWithIdle("holder", 30)
	holder.Level, holder.Purpose = "strong", "planner"
	m.Start(context.Background(), newRecorder(), holder)
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 30)
	waiter.Level, waiter.Purpose = "fast", "decide"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	time.Sleep(120 * time.Millisecond) // past the floor, with the holder still holding
	release()
	awaitRequest(t, arrived)
	if end := rec.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("the waiting call ended %q", end.GetErrorCode())
	}

	out := logs.String()
	for _, want := range []string{
		`"msg":"model call waited for a concurrency slot"`,
		`"request_id":"waiter"`,
		`"call_level":"fast"`,
		`"purpose":"decide"`,
		`"model":"m"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"request_id":"holder"`) {
		t.Errorf("a call that took a free slot must not be logged as having waited:\n%s", out)
	}
}

// TestARefusedWaitIsALoudLine. The refusal is a FAILED call -- the engine
// does not reroute it -- so it is the line an owner most needs, and it was
// a debug line.
func TestARefusedWaitIsALoudLine(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory: inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:    func(string) string { return "" },
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 1)
	waiter.Level = "fast"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	if end := rec.wait(t); end.GetErrorCode() != CodeConcurrencyExceeded {
		t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeConcurrencyExceeded)
	}
	out := logs.String()
	for _, want := range []string{
		`"level":"WARN"`,
		`"msg":"model call refused after waiting for a concurrency slot"`,
		`"request_id":"waiter"`,
		`"error_code":"model_concurrency_exceeded"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %s:\n%s", want, out)
		}
	}
}

// TestACallThatDoesNotWaitSaysNothing. Every call logging would bury the
// ones that matter.
func TestACallThatDoesNotWaitSaysNothing(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"hi"}, false)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory: inventoryWith(ollamaModel(srv.URL, "llama3.1:8b", models.Attributes{MaxConcurrent: 2})),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:    func(string) string { return "" },
	})
	rec := newRecorder()
	m.Start(context.Background(), rec, start("r1", "llama3.1:8b", KindChat))
	rec.wait(t)
	if out := logs.String(); strings.Contains(out, "concurrency slot") {
		t.Errorf("a call that took a free slot logged about slots:\n%s", out)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/modelcall/ -run 'TestAWaitedSlot|TestARefusedWait|TestACallThatDoesNotWait' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall [build failed]
internal/worker/modelcall/slotwait_test.go:49:3: unknown field SlotWaitLogFloor in struct literal of type Options
```

- [ ] **Step 3: Write the implementation**

Modify `internal/worker/modelcall/session.go`:

```diff
--- a/internal/worker/modelcall/session.go
+++ b/internal/worker/modelcall/session.go
@@ -93,6 +93,10 @@
 	// builds a private one, which is the whole machine when this Manager
 	// is the only one.
 	Limiter *Limiter
+	// SlotWaitLogFloor is the wait below which a call that had to queue for
+	// a concurrency slot is not logged. Zero is one second: a slot that
+	// frees within a second is the machine doing its job.
+	SlotWaitLogFloor time.Duration
 }
 
 // Manager owns the live model calls on this worker.
@@ -103,6 +107,8 @@
 	getenv    func(string) string
 	limiter   *Limiter
 
+	slotWaitFloor time.Duration
+
 	mu   sync.Mutex
 	live map[string]*call
 }
@@ -110,6 +116,11 @@
 type call struct {
 	requestID string
 	cancel    context.CancelFunc
+	// level and purpose are what the engine said about the call. They steer
+	// nothing here -- the router chose the model -- and exist so the line
+	// logged when a call waits for a slot can say which kind of work was
+	// kept waiting.
+	level, purpose string
 
 	mu sync.Mutex
 	// modelID is set once the model RESOLVES and its slot is taken in the
@@ -142,13 +153,18 @@
 	if limiter == nil {
 		limiter = NewLimiter()
 	}
+	floor := opts.SlotWaitLogFloor
+	if floor <= 0 {
+		floor = time.Second
+	}
 	return &Manager{
-		logger:    opts.Logger,
-		inventory: opts.Inventory,
-		http:      client,
-		getenv:    getenv,
-		limiter:   limiter,
-		live:      make(map[string]*call),
+		logger:        opts.Logger,
+		inventory:     opts.Inventory,
+		http:          client,
+		getenv:        getenv,
+		limiter:       limiter,
+		slotWaitFloor: floor,
+		live:          make(map[string]*call),
 	}
 }
 
@@ -191,7 +207,7 @@
 	// its own timeout -- exactly the outcome cancel exists to prevent.
 	limits := limitsFrom(start.GetLimits())
 	callCtx, cancel := context.WithTimeout(ctx, limits.timeout)
-	c := &call{requestID: requestID, cancel: cancel}
+	c := &call{requestID: requestID, cancel: cancel, level: start.GetLevel(), purpose: start.GetPurpose()}
 
 	m.mu.Lock()
 	_, duplicate := m.live[requestID]
@@ -442,6 +458,7 @@
 	if m.claim(c, info, ceiling) {
 		return nil
 	}
+	began := time.Now()
 	m.logger.Debug("model call waiting for a concurrency slot", "request_id", c.requestID, "model", info.ID)
 	deadline := time.NewTimer(limits.idle)
 	defer deadline.Stop()
@@ -452,6 +469,7 @@
 		// Re-checked AFTER taking the channel, so a release between the
 		// failed claim and the wait is never missed.
 		if m.claim(c, info, ceiling) {
+			m.slotWaited(c, info, time.Since(began))
 			return nil
 		}
 		select {
@@ -461,6 +479,10 @@
 				return refuse(CodeWorkerStopped, "the stream to the cluster went while this call waited for a concurrency slot")
 			}
 		case <-deadline.C:
+			m.logger.Warn("model call refused after waiting for a concurrency slot",
+				"request_id", c.requestID, "model", info.ID, "call_level", c.level, "purpose", c.purpose,
+				"waited_ms", time.Since(began).Milliseconds(), "model_max", info.MaxConcurrent,
+				"error_code", CodeConcurrencyExceeded)
 			return refuse(CodeConcurrencyExceeded, concurrencyRefusal(info, ceiling, limits.idle))
 		case <-ctx.Done():
 			finish, code, detail := m.classify(c, ctx.Err())
@@ -469,6 +491,23 @@
 			return end
 		}
 	}
+}
+
+// slotWaited says that a call waited for its slot, and for how long.
+//
+// IT IS THE EVIDENCE for a decision this cockpit deliberately has not
+// made: reserving slots for the fast level (D6 in the fleet-tuning design,
+// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md). Whether
+// background work starves fast work here is a fact about this machine's
+// traffic, and level and purpose are the only things the call says about
+// itself.
+func (m *Manager) slotWaited(c *call, info models.Info, waited time.Duration) {
+	if waited < m.slotWaitFloor {
+		return
+	}
+	m.logger.Info("model call waited for a concurrency slot",
+		"request_id", c.requestID, "model", info.ID, "call_level", c.level, "purpose", c.purpose,
+		"waited_ms", waited.Milliseconds(), "model_max", info.MaxConcurrent)
 }
 
 // claim takes the slot if one is free. Taking it and recording which one
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/modelcall/ -run 'TestAWaitedSlot|TestARefusedWait|TestACallThatDoesNotWait' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestAWaitedSlotIsLoggedWithItsLevelAndPurpose (0.12s)
--- PASS: TestARefusedWaitIsALoudLine (1.00s)
--- PASS: TestACallThatDoesNotWaitSaysNothing (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	1.129s
```

- [ ] **Step 5: Run the package, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/modelcall/
go test ./internal/worker/modelcall/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/modelcall/
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(modelcall): log when a call waits for a concurrency slot" \
  -m "A wait of a second or more is an info line with the call's model, level and purpose; a refusal after waiting is a warning. Whether background work starves fast work on this machine is the evidence for reserving slots per level, and it was a debug line before." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: The managed unit sets the slots and the worker reads them back

**Files:**
- Create: `internal/worker/inference/parallel.go`
- Modify: `internal/worker/inference/stage.go`
- Modify: `internal/worker/inference/plan.go`
- Modify: `internal/worker/modelinventory.go` (newDiscoverer)
- Test: `internal/worker/inference/parallel_test.go`
- Test: `internal/worker/inference/fuzz_test.go` (add one target)
- Test: `internal/worker/models_tuning_wiring_test.go` (add one test)

**Interfaces:**
- Consumes: `models.Discoverer.ManagedParallel` (Task 4); `newDiscoverer` (Task 5).
- Produces: `inference.DefaultParallel(class string) int` (2 for classes "24" and "32", else 1); `inference.Stage.Parallel int`; `inference.UnitParallel(texts ...string) int` (the unit then its drop-ins in lexical order; the last `OLLAMA_NUM_PARALLEL` wins; an empty `Environment=` resets; zero when unusable); `inference.ManagedParallel() int` (reads `~/.config/systemd/user/memql-ollama.service` and `memql-ollama.service.d/*.conf`; zero when there is no unit); `newDiscoverer` now sets `ManagedParallel`.

Decision D3. Ollama reads `OLLAMA_NUM_PARALLEL` in its own process and the worker advertises from its own, in two separate services; the unit is the one place both can be made to agree. A fresh native install on a 24 or 32 GB machine writes two slots; every other class writes the unit it always wrote; a runtime that is already serving is never rewritten (`decideLinuxNative` returns first), which is why the owner's laptop is edited by hand in Task 13.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/inference/parallel_test.go`:

```go
package inference

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
)

// parallel_test.go pins the one number the managed Ollama and the cockpit
// have to agree on: how many requests a model takes at once. Ollama
// decides it from OLLAMA_NUM_PARALLEL in the unit; the worker advertises it
// as the `max=` on the model label, and the engine rations by that label.

func TestDefaultParallel(t *testing.T) {
	for class, want := range map[string]int{
		// Measured on a 24 GB card: two slots at a 16K window leave about
		// 2.7 GB free (spec, section 5). Higher classes have more room but
		// no measurement behind them.
		"24": 2, "32": 2,
		"unsupported": 1, "16": 1, "64": 1, "128": 1, "": 1, "nonsense": 1,
	} {
		if got := DefaultParallel(class); got != want {
			t.Errorf("DefaultParallel(%q) = %d, want %d", class, got, want)
		}
	}
}

func TestTheNativeUnitCarriesTheParallelSlots(t *testing.T) {
	s := stageIn("/home/op")
	if unit := s.Unit(); strings.Contains(unit, "OLLAMA_NUM_PARALLEL") {
		t.Errorf("a stage with no parallel setting must write the unit it always wrote:\n%s", unit)
	}
	s.Parallel = 1
	if unit := s.Unit(); strings.Contains(unit, "OLLAMA_NUM_PARALLEL") {
		t.Errorf("one slot is Ollama's own default and needs no line:\n%s", unit)
	}
	s.Parallel = 2
	unit := s.Unit()
	if !strings.Contains(unit, "\nEnvironment=OLLAMA_NUM_PARALLEL=2\n") {
		t.Errorf("the unit does not set the slots:\n%s", unit)
	}
	// Beside the other Environment lines, inside [Service].
	if strings.Index(unit, "OLLAMA_NUM_PARALLEL") < strings.Index(unit, "[Service]") ||
		strings.Index(unit, "OLLAMA_NUM_PARALLEL") > strings.Index(unit, "[Install]") {
		t.Errorf("the setting is outside [Service]:\n%s", unit)
	}
}

// The consent covers the stage, so the stage has to say what it will set.
func TestStageLinesNameTheParallelSlots(t *testing.T) {
	s := stageIn("/home/op")
	if got := strings.Join(s.Lines(), "\n"); strings.Contains(got, "at a time") {
		t.Errorf("one slot needs no mention:\n%s", got)
	}
	s.Parallel = 2
	if got := strings.Join(s.Lines(), "\n"); !strings.Contains(got, "2 requests at a time") {
		t.Errorf("the stage must say how many requests it lets Ollama take:\n%s", got)
	}
}

// A fresh native install on a 24 GB card gets the class default; on any
// other class it writes the unit it always wrote. A machine whose runtime
// is already serving is never touched (decideLinuxNative returns first),
// which is why the owner's laptop is changed by hand and documented.
func TestDecideGivesTheStageTheClassDefault(t *testing.T) {
	host := func(vramMiB uint64) Host {
		return Host{
			GOOS: "linux", GOARCH: "amd64",
			Floor:    metFloor("NVIDIA GeForce RTX 5090 Laptop GPU"),
			Native:   nativeFacts(GPUVendorNVIDIA),
			LookPath: lookPath("systemctl", "nvidia-smi"),
			Hardware: hardware.Inventory{
				GPU: &hardware.GPU{Name: "NVIDIA GeForce RTX 5090 Laptop GPU", VRAMBytes: vramMiB << 20, Backend: hardware.BackendCUDA},
			},
		}
	}
	for name, tc := range map[string]struct {
		vramMiB   uint64
		wantClass string
		want      int
	}{
		"the reference laptop":   {24463, "24", 2},
		"a 32 GB card":           {32607, "32", 2},
		"a 16 GB card":           {16303, "16", 1},
		"an 8 GB card":           {8188, "unsupported", 1},
		"a card with no reading": {0, "unsupported", 1},
	} {
		t.Run(name, func(t *testing.T) {
			p := Decide(host(tc.vramMiB))
			if p.Stage == nil {
				t.Fatalf("no stage: %q", p.Refusal)
			}
			if p.MachineClass != tc.wantClass {
				t.Fatalf("class = %q, want %q", p.MachineClass, tc.wantClass)
			}
			if p.Stage.Parallel != tc.want {
				t.Errorf("Stage.Parallel = %d, want %d", p.Stage.Parallel, tc.want)
			}
		})
	}
}

func TestUnitParallel(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  int
	}{
		{"a plain line", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n"}, 2},
		{"quoted whole", []string{"[Service]\nEnvironment=\"OLLAMA_NUM_PARALLEL=3\"\n"}, 3},
		{"quoted single", []string{"[Service]\nEnvironment='OLLAMA_NUM_PARALLEL=3'\n"}, 3},
		{"several assignments on one line", []string{"[Service]\nEnvironment=OLLAMA_HOST=127.0.0.1:11434 OLLAMA_NUM_PARALLEL=4 OLLAMA_KV_CACHE_TYPE=q8_0\n"}, 4},
		{"the last assignment wins", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"}, 3},
		{"a drop-in overrides the unit", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"}, 3},
		{"an empty Environment resets the list", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n", "[Service]\nEnvironment=\n"}, 0},
		{"another section is not the service", []string{"[Unit]\nEnvironment=OLLAMA_NUM_PARALLEL=9\n"}, 0},
		{"a comment is not a setting", []string{"[Service]\n# Environment=OLLAMA_NUM_PARALLEL=2\n; Environment=OLLAMA_NUM_PARALLEL=2\n"}, 0},
		{"a similar name is not the variable", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL_X=5\n"}, 0},
		// systemd would hand Ollama the text, and Ollama ignores what it
		// cannot parse, so an unusable last word leaves nothing to claim.
		{"a word is not a number", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=two\n"}, 0},
		{"zero is not a ceiling", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=0\n"}, 0},
		{"negative is not a ceiling", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=-2\n"}, 0},
		{"an unusable later value does not keep the earlier one", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\nEnvironment=OLLAMA_NUM_PARALLEL=lots\n"}, 0},
		{"no unit at all", nil, 0},
		{"a unit that says nothing", []string{"[Service]\nExecStart=/usr/bin/env ollama serve\n"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnitParallel(tt.texts...); got != tt.want {
				t.Errorf("UnitParallel = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestUnitParallelReadsWhatTheStageWrote. The cockpit reads back the line
// it wrote, whatever else the operator's paths put in the unit.
func TestUnitParallelReadsWhatTheStageWrote(t *testing.T) {
	s := stageIn("/home/A Person%u")
	s.ModelsDir = "/mnt/a \"quoted\" path%u/line\nnext"
	s.Parallel = 2
	if got := UnitParallel(s.Unit()); got != 2 {
		t.Errorf("UnitParallel(Stage.Unit()) = %d, want 2:\n%s", got, s.Unit())
	}
}

// TestManagedParallelAtReadsTheUnitAndItsDropIns. `systemctl --user edit`
// writes a drop-in, which is the way systemd documents changing a unit, so
// a cockpit that read only the unit would ignore the owner's edit.
func TestManagedParallelAtReadsTheUnitAndItsDropIns(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "memql-ollama.service")

	if got := managedParallelAt(unit); got != 0 {
		t.Errorf("no unit file -> %d, want 0 (no managed runtime)", got)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(unit, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n")
	if got := managedParallelAt(unit); got != 2 {
		t.Errorf("the unit alone -> %d, want 2", got)
	}
	write(unit+".d/10-first.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n")
	write(unit+".d/20-second.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=4\n")
	if got := managedParallelAt(unit); got != 4 {
		t.Errorf("drop-ins apply in lexical order, the last one winning -> %d, want 4", got)
	}
	// Not a .conf: systemd ignores it, so does the cockpit.
	write(unit+".d/30-notes.txt", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=9\n")
	if got := managedParallelAt(unit); got != 4 {
		t.Errorf("a non-.conf file in the drop-in directory changed the answer to %d", got)
	}
}
```

Modify `internal/worker/inference/fuzz_test.go` (shown as a diff; add exactly this):

```diff
--- a/internal/worker/inference/fuzz_test.go
+++ b/internal/worker/inference/fuzz_test.go
@@ -114,3 +114,45 @@
 		}
 	})
 }
+
+// FuzzUnitParallel. The text is a unit file an owner may have edited by
+// hand, and the answer becomes the ceiling the whole fleet is told, so the
+// failure that matters is a number the unit does not say. Three properties.
+//
+//   - THE ANSWER IS ZERO OR A POSITIVE COUNT, never a negative one: a
+//     negative ceiling reaching the engine's rationing has no meaning.
+//
+//   - A RESET RESETS. An empty Environment= under [Service], appended after
+//     any text at all, leaves the unit saying nothing: that is what systemd
+//     does with it, and a cockpit that kept the earlier number would
+//     advertise slots the unit no longer gives Ollama.
+//
+//   - A COMMENT IS NOT A SETTING. A comment line appended after any text
+//     never changes the answer, however it mentions the variable.
+func FuzzUnitParallel(f *testing.F) {
+	for _, seed := range []string{
+		"", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n",
+		"[Service]\nEnvironment=\"OLLAMA_NUM_PARALLEL=3\"\n",
+		"[Service]\nEnvironment=A=1 OLLAMA_NUM_PARALLEL=4 B=2\n",
+		"[Unit]\nEnvironment=OLLAMA_NUM_PARALLEL=9\n",
+		"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=-2\n",
+		"[Service]\nEnvironment='OLLAMA_NUM_PARALLEL=\n",
+		"[Service]\nEnvironment=\\\"OLLAMA_NUM_PARALLEL=5\n",
+		"[Service\nEnvironment=OLLAMA_NUM_PARALLEL=2\n",
+		"\x00[Service]\r\nEnvironment=OLLAMA_NUM_PARALLEL=2\r\n",
+	} {
+		f.Add(seed)
+	}
+	f.Fuzz(func(t *testing.T, unit string) {
+		n := UnitParallel(unit)
+		if n < 0 {
+			t.Fatalf("UnitParallel(%q) = %d", unit, n)
+		}
+		if got := UnitParallel(unit, "[Service]\nEnvironment=\n"); got != 0 {
+			t.Fatalf("UnitParallel(%q + a reset) = %d, want 0", unit, got)
+		}
+		if got := UnitParallel(unit + "\n# Environment=OLLAMA_NUM_PARALLEL=7\n"); got != n {
+			t.Fatalf("a trailing comment moved UnitParallel(%q) from %d to %d", unit, n, got)
+		}
+	})
+}
```

Modify `internal/worker/models_tuning_wiring_test.go` (shown as a diff; add exactly this):

```diff
--- a/internal/worker/models_tuning_wiring_test.go
+++ b/internal/worker/models_tuning_wiring_test.go
@@ -131,3 +131,28 @@
 		}
 	}
 }
+
+// TestNewDiscovererReadsTheManagedUnit. The ceiling a fresh install
+// advertises is the one its unit gives Ollama, with no second place to set
+// it: newDiscoverer is what hands discovery the unit.
+func TestNewDiscovererReadsTheManagedUnit(t *testing.T) {
+	home := t.TempDir()
+	t.Setenv("HOME", home)
+	d := newDiscoverer()
+	if d.ManagedParallel == nil {
+		t.Fatal("newDiscoverer leaves discovery without the managed unit")
+	}
+	if got := d.ManagedParallel(); got != 0 {
+		t.Errorf("no unit on this machine -> %d, want 0", got)
+	}
+	unit := filepath.Join(home, ".config", "systemd", "user", "memql-ollama.service")
+	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
+		t.Fatal(err)
+	}
+	if err := os.WriteFile(unit, []byte("[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"), 0o644); err != nil {
+		t.Fatal(err)
+	}
+	if got := d.ManagedParallel(); got != 3 {
+		t.Errorf("a unit declaring 3 slots -> %d, want 3", got)
+	}
+}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/inference/ ./internal/worker/ -run 'TestDefaultParallel|TestTheNativeUnitCarries|TestStageLinesNameTheParallel|TestDecideGivesTheStage|TestUnitParallel|TestManagedParallelAt|FuzzUnitParallel|TestNewDiscovererReadsTheManagedUnit' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/inference [build failed]
--- FAIL: TestNewDiscovererReadsTheManagedUnit (0.00s)
    models_tuning_wiring_test.go:143: newDiscoverer leaves discovery without the managed unit
FAIL
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker	0.018s
FAIL
# github.com/znasllc-io/memql-cockpit/internal/worker/inference [github.com/znasllc-io/memql-cockpit/internal/worker/inference.test]
internal/worker/inference/fuzz_test.go:147:8: undefined: UnitParallel
```

- [ ] **Step 3: Write the implementation**

Create `internal/worker/inference/parallel.go`:

```go
package inference

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// parallel.go: how many requests a model takes at once, written in one
// place and read back from it.
//
// Ollama decides from OLLAMA_NUM_PARALLEL in the unit this package writes.
// The worker advertises the same number as the `max=` on every model label
// and enforces it (modelcall.Limiter), and the engine rations by it. When
// the two came from different places they drifted: the unit never set it,
// the worker read it from its OWN environment, and a person who exported it
// to the wrong process got a ceiling nothing enforced. So the unit is the
// source, and UnitParallel reads it back. Design record:
// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md, D3.

// DefaultParallel is the number of slots a fresh native install gives
// Ollama for this machine class. Every slot costs memory, and the measured
// cost is the whole basis for the table.
//
// MEASURED on an RTX 5090 Laptop (24 GB) with the qwen3.8:27b the 24 and 32
// classes pull, from llama-server's own fit report in the unit's log: one
// slot at a 16K window projects 17.0 GB, and each further slot adds 748 MiB
// of recurrent state plus 38 KiB per token of cache -- 1.36 GB at 16K,
// 1.96 GB at 32K. The 0.6B embedder beside it is estimated, not measured,
// at 1.4 GB and 0.5 GB per further slot. Two slots at 16K then leave about
// 2.7 GB of the 22.4 GB the card reports free; two at 32K leave about
// 1.4 GB, under the 1.9 GB the report itself says it wants to leave, so
// the fit step would start taking layers off the GPU (not observed; the
// rollout reads the report). So the default is two for the classes that
// pull that model, and nothing is claimed for a class nobody measured:
// one is Ollama's own default and costs nothing to keep.
func DefaultParallel(class string) int {
	switch class {
	case "24", "32":
		return 2
	default:
		return 1
	}
}

// parallelEnvironment is the unit line for the stage's slots, or nothing.
// One slot is Ollama's default, so a stage that wants one writes the unit
// it always wrote.
func (s Stage) parallelEnvironment() string {
	if s.Parallel <= 1 {
		return ""
	}
	return "# Requests a model takes at once. The worker reads this line and advertises the same ceiling.\n" +
		"Environment=OLLAMA_NUM_PARALLEL=" + strconv.Itoa(s.Parallel) + "\n"
}

// UnitParallel is the OLLAMA_NUM_PARALLEL a unit declares, read the way
// systemd merges a unit with its drop-ins: texts are the main unit first
// and then each drop-in in lexical order; the last assignment of the
// variable wins; and an empty Environment= resets the list. Zero means the
// unit says nothing usable -- no line, a number below one, or a word, which
// Ollama ignores the same way.
//
// Only [Service] counts and comments are skipped. EnvironmentFile= is not
// followed: this cockpit never writes one for Ollama, and an owner who did
// has told the cockpit through models.parallel instead.
func UnitParallel(texts ...string) int {
	value := ""
	for _, text := range texts {
		inService := false
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
				continue
			case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
				inService = line == "[Service]"
				continue
			}
			if !inService {
				continue
			}
			key, rest, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != "Environment" {
				continue
			}
			rest = strings.TrimSpace(rest)
			if rest == "" {
				value = ""
				continue
			}
			for _, word := range unitWords(rest) {
				if name, v, ok := strings.Cut(word, "="); ok && name == "OLLAMA_NUM_PARALLEL" {
					value = strings.TrimSpace(v)
				}
			}
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// unitWords splits the right-hand side of an Environment= line the way
// systemd does for the cases a unit this cockpit wrote, or an owner edited,
// contains: whitespace separates assignments, a pair of single or double
// quotes keeps spaces inside one, and a backslash keeps the next character
// as it is.
func unitWords(s string) []string {
	var (
		words   []string
		cur     strings.Builder
		quote   rune
		inWord  bool
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// ManagedParallel is the slots the unit this cockpit manages declares for
// its Ollama, or zero when this machine has no such unit (macOS, Docker, an
// Ollama somebody started by hand). The worker hands it to discovery as
// models.Discoverer.ManagedParallel.
func ManagedParallel() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	return managedParallelAt(DefaultNativeFacts(home, "").UnitPath)
}

func managedParallelAt(unitPath string) int {
	main, err := os.ReadFile(unitPath)
	if err != nil {
		return 0
	}
	texts := []string{string(main)}
	dropIns, _ := filepath.Glob(unitPath + ".d/*.conf")
	sort.Strings(dropIns)
	for _, path := range dropIns {
		if b, err := os.ReadFile(path); err == nil {
			texts = append(texts, string(b))
		}
	}
	return UnitParallel(texts...)
}
```

Modify `internal/worker/inference/stage.go`:

```diff
--- a/internal/worker/inference/stage.go
+++ b/internal/worker/inference/stage.go
@@ -130,6 +130,11 @@
 	// has no authentication and nothing off-box needs the port -- the
 	// cluster reaches this machine over the worker's outbound stream.
 	Listen string
+	// Parallel is the requests Ollama takes at once per model, written as
+	// OLLAMA_NUM_PARALLEL. Zero and one write nothing: one is Ollama's own
+	// default, and a unit that says it again would only be a second place
+	// for the number to be wrong. See DefaultParallel.
+	Parallel int
 }
 
 // Archive is one release asset.
@@ -212,7 +217,11 @@
 		out = append(out, "download "+a.Name+" from "+strings.TrimSuffix(a.URL(), "/"+a.Name)+
 			" and unpack it into "+s.RuntimeDir+", checked against the release's "+ollamaChecksumFile)
 	}
-	out = append(out, "write "+s.UnitPath+" (Ollama as your user, listening on "+s.Listen+", models in "+s.ModelsDir+")")
+	detail := "Ollama as your user, listening on " + s.Listen + ", models in " + s.ModelsDir
+	if s.Parallel > 1 {
+		detail += ", " + strconv.Itoa(s.Parallel) + " requests at a time"
+	}
+	out = append(out, "write "+s.UnitPath+" ("+detail+")")
 	return out
 }
 
@@ -240,6 +249,7 @@
 		// attention, which it needs, is the runtime's default where the
 		// device supports it (2026-09-08 record, D9).
 		"Environment=OLLAMA_KV_CACHE_TYPE=q8_0\n" +
+		s.parallelEnvironment() +
 		"Restart=on-failure\n" +
 		"RestartSec=3\n" +
 		"StandardOutput=append:" + strings.ReplaceAll(s.LogPath, "%", "%%") + "\n" +
```

Modify `internal/worker/inference/plan.go`:

```diff
--- a/internal/worker/inference/plan.go
+++ b/internal/worker/inference/plan.go
@@ -495,6 +495,7 @@
 		LogPath:    n.LogPath,
 		UnitPath:   n.UnitPath,
 		Listen:     nativeListen,
+		Parallel:   DefaultParallel(p.MachineClass),
 	}
 	switch {
 	case n.Installed:
```

Modify `internal/worker/modelinventory.go`:

```diff
--- a/internal/worker/modelinventory.go
+++ b/internal/worker/modelinventory.go
@@ -6,6 +6,7 @@
 	"sync"
 	"time"
 
+	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
 	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
 )
@@ -78,7 +79,7 @@
 // does not advertise. TestEveryDiscoveryIsBuiltInOnePlace holds the seven
 // to it.
 func newDiscoverer() *models.Discoverer {
-	return &models.Discoverer{}
+	return &models.Discoverer{ManagedParallel: inference.ManagedParallel}
 }
 
 // modelsRequest is what this machine's policy asks discovery: which models
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/inference/ ./internal/worker/ -run 'TestDefaultParallel|TestTheNativeUnitCarries|TestStageLinesNameTheParallel|TestDecideGivesTheStage|TestUnitParallel|TestManagedParallelAt|FuzzUnitParallel|TestNewDiscovererReadsTheManagedUnit' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestDefaultParallel (0.00s)
--- PASS: TestTheNativeUnitCarriesTheParallelSlots (0.00s)
--- PASS: TestStageLinesNameTheParallelSlots (0.00s)
--- PASS: TestDecideGivesTheStageTheClassDefault (0.00s)
--- PASS: TestUnitParallel (0.00s)
--- PASS: TestUnitParallelReadsWhatTheStageWrote (0.00s)
--- PASS: TestManagedParallelAtReadsTheUnitAndItsDropIns (0.00s)
--- PASS: FuzzUnitParallel (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/inference	0.007s
--- PASS: TestNewDiscovererReadsTheManagedUnit (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	0.019s
```

- [ ] **Step 5: Run the packages, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/inference/ ./internal/worker/
go test ./internal/worker/inference/ ./internal/worker/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/inference	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/inference/ \
  internal/worker/modelinventory.go \
  internal/worker/models_tuning_wiring_test.go
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(inference): the managed unit sets the slot count and the worker reads it back" \
  -m "OLLAMA_NUM_PARALLEL lives in the Ollama process and the advertised ceiling in the worker, two services, so the unit becomes the source and UnitParallel reads it back, drop-ins included. A fresh install on a 24 or 32 GB machine writes two slots, the most the measured 24 GB budget holds at a 16K window." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: memql worker models shows the tuning and what Ollama holds

**Files:**
- Create: `internal/worker/models/loaded.go`
- Create: `internal/worker/models_tuning_report.go`
- Modify: `internal/worker/models_cmd.go`
- Test: `internal/worker/models/loaded_test.go`
- Test: `internal/worker/models_cmd_tuning_test.go`

**Interfaces:**
- Consumes: `Discoverer.ParallelSource`, `Info.KeepAlive/WorkingContext` (Task 4); `Policy.ModelsTuning()` and `ModelsTuningProblems()` (Task 3); `newDiscoverer` (Task 5).
- Produces: `models.Loaded{ID; SizeBytes, VRAMBytes int64; ContextLength int; ExpiresAt time.Time}` with `Forever(now)` and `GPUPercent()`; `(*Discoverer).Loaded(ctx) ([]Loaded, error)` (GET `/api/ps`); `tuningReport` and `printModelTuning(w, inv, report)`; a "Residency and concurrency" section at the end of `memql worker models`.

A setting is an intention until the runtime shows it kept. The report prints the ceiling and where it came from, what is configured to stay loaded, and what Ollama holds now: size, share on the GPU, window, expiry. A model mostly on the CPU is called out with the command that fixes it, because residency makes a bad placement last (spec D4, D7). An `/api/ps` that does not answer is a line, not an error.

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/models/loaded_test.go`:

```go
package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// psFixture is a recorded GET /api/ps from Ollama 0.34.2: one model kept
// forever (expires_at is the far future Ollama uses for -1) and one kept
// for a finite time, the second only partly on the GPU.
const psFixture = `{"models":[
 {"name":"qwen3.8:27b","model":"qwen3.8:27b","size":18400000000,"digest":"d1","details":{"family":"qwen35"},
  "expires_at":"2319-01-28T19:03:29.099855393Z","size_vram":18400000000,"context_length":16384,"runner":"llamacpp"},
 {"name":"big:70b","model":"big:70b","size":40000000000,"digest":"d2","details":{"family":"llama"},
  "expires_at":"2026-10-18T14:30:00-07:00","size_vram":24000000000,"context_length":8192,"runner":"llamacpp"}
]}`

func psStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoaded(t *testing.T) {
	srv := psStub(t, 200, psFixture)
	got, err := discovererFor(srv.URL, nil).Loaded(context.Background())
	if err != nil {
		t.Fatalf("Loaded: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Loaded = %+v, want two models", got)
	}
	now := time.Date(2026, 10, 18, 12, 30, 0, 0, time.FixedZone("PDT", -7*3600))

	big := got[0]
	if big.ID != "qwen3.8:27b" || big.SizeBytes != 18400000000 || big.ContextLength != 16384 {
		t.Errorf("first model = %+v", big)
	}
	if !big.Forever(now) {
		t.Errorf("expires_at %v is Ollama's spelling of keep_alive -1, and must read as forever", big.ExpiresAt)
	}
	if p := big.GPUPercent(); p != 100 {
		t.Errorf("GPUPercent = %d, want 100", p)
	}

	part := got[1]
	if part.Forever(now) {
		t.Errorf("a model expiring in two hours read as forever")
	}
	if d := part.ExpiresAt.Sub(now); d != 2*time.Hour {
		t.Errorf("expires in %v, want 2h", d)
	}
	if p := part.GPUPercent(); p != 60 {
		t.Errorf("GPUPercent = %d, want 60 (24 GB of 40 GB on the card)", p)
	}
}

// TestLoadedNothingLoadedIsAnAnswer. An Ollama with nothing in memory is
// the ordinary state of an untuned machine, and it is not an error.
func TestLoadedNothingLoadedIsAnAnswer(t *testing.T) {
	srv := psStub(t, 200, `{"models":[]}`)
	got, err := discovererFor(srv.URL, nil).Loaded(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("Loaded = %+v, %v; want an empty list and no error", got, err)
	}
}

// TestLoadedSaysWhyItCouldNotAsk. A runtime that is down, or an Ollama too
// old to have /api/ps, is a sentence for the report and never a failure of
// the command that prints it.
func TestLoadedSaysWhyItCouldNotAsk(t *testing.T) {
	for name, srv := range map[string]*httptest.Server{
		"a route Ollama does not have": psStub(t, 404, "not found"),
		"a body that is not JSON":      psStub(t, 200, "<html>"),
	} {
		if _, err := discovererFor(srv.URL, nil).Loaded(context.Background()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := discovererFor(dead.URL, nil).Loaded(context.Background()); err == nil {
		t.Error("a runtime that is down: want an error")
	}
}
```

Create `internal/worker/models_cmd_tuning_test.go`:

```go
package worker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

var reportNow = time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)

func renderTuning(t *testing.T, inv models.Inventory, r tuningReport) string {
	t.Helper()
	if r.Now.IsZero() {
		r.Now = reportNow
	}
	var b strings.Builder
	printModelTuning(&b, inv, r)
	return b.String()
}

func tunedInventory() models.Inventory {
	chat := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	chat.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	chat.WorkingContext = 16384
	embed := offeredModel("qwen3-embedding:0.6b", models.Attributes{Embeddings: true, MaxConcurrent: 2})
	embed.KeepAlive = models.KeepAlive{Set: true, Forever: true}
	plain := offeredModel("other:7b", models.Attributes{MaxConcurrent: 2})
	return servingInventory(chat, embed, plain)
}

// TestModelsTuningReport_SaysWhatIsConfigured. The command exists to
// answer "did it take", and the first half of that is what the cockpit
// will tell Ollama.
func TestModelsTuningReport_SaysWhatIsConfigured(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the memql-ollama.service unit"})
	for _, want := range []string{
		"Requests per model: 2, from the memql-ollama.service unit.",
		"qwen3.8:27b",
		"kept loaded for 2h0m0s, at a working context of 16384",
		"qwen3-embedding:0.6b",
		"kept loaded forever",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	// A model with no entry is not listed as configured.
	if strings.Contains(out, "other:7b") {
		t.Errorf("a model with no models.resident entry was listed:\n%s", out)
	}
}

func TestModelsTuningReport_NothingConfiguredSaysWhatOllamaDoesInstead(t *testing.T) {
	inv := servingInventory(offeredModel("llama3.1:8b", models.Attributes{MaxConcurrent: 1}))
	out := renderTuning(t, inv, tuningReport{Parallel: 1, ParallelSource: "the default"})
	for _, want := range []string{
		"nothing is configured to stay loaded",
		"five minutes after its last call",
		"models.resident",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// TestModelsTuningReport_SaysWhatOllamaHoldsNow is the second half: the
// settings are only intentions until the runtime shows them kept.
func TestModelsTuningReport_SaysWhatOllamaHoldsNow(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		Loaded: []models.Loaded{
			{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 18_400_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(118 * time.Minute)},
			{ID: "qwen3-embedding:0.6b", SizeBytes: 1_400_000_000, VRAMBytes: 1_400_000_000, ContextLength: 8192, ExpiresAt: reportNow.AddDate(300, 0, 0)},
		},
	})
	for _, want := range []string{
		"Ollama has loaded now:",
		"qwen3.8:27b",
		"18.4 GB, 100% on the GPU, window 16384, unloads in 1h58m",
		"1.4 GB, 100% on the GPU, window 8192, stays loaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// TestModelsTuningReport_AModelPartlyOnTheCPUIsCalledOut. A resident model
// loaded while the card was busy is the failure residency makes
// permanent, and the line has to say what to do about it.
func TestModelsTuningReport_AModelPartlyOnTheCPUIsCalledOut(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		Loaded: []models.Loaded{
			{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 6_500_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)},
		},
	})
	for _, want := range []string{
		"35% on the GPU",
		"The rest runs on the CPU, which is slow, and stays that way until the model reloads.",
		"ollama stop qwen3.8:27b",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

func TestModelsTuningReport_NothingLoadedAndNothingReadable(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the default"})
	if !strings.Contains(out, "Ollama has nothing loaded now") {
		t.Errorf("an empty runtime must read as empty:\n%s", out)
	}
	out = renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		LoadedErr: errors.New("no answer from Ollama's /api/ps at http://127.0.0.1:11434 (status 404)"),
	})
	// Review focus: an unreadable runtime is a line in the report, never
	// an error that stops the command from printing the rest.
	if !strings.Contains(out, "Could not read what Ollama has loaded: no answer from Ollama's /api/ps") {
		t.Errorf("the reason must be printed:\n%s", out)
	}
	if !strings.Contains(out, "Requests per model: 2") {
		t.Errorf("the rest of the report must survive an unreadable runtime:\n%s", out)
	}
}

func TestModelsTuningReport_PrintsTheProblems(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 1, ParallelSource: "the default",
		Problems: []string{"models.parallel: 500 is outside 1 to 64 and is ignored"},
	})
	if !strings.Contains(out, "Problems with models.parallel and models.resident:") ||
		!strings.Contains(out, "- models.parallel: 500 is outside 1 to 64 and is ignored") {
		t.Errorf("problems are not printed:\n%s", out)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ ./internal/worker/ -run 'TestLoaded|TestModelsTuningReport' -count=1
```

Expected: FAIL, and the first lines say why (the test names something the code does not have yet):

```text
FAIL	github.com/znasllc-io/memql-cockpit/internal/worker/models [build failed]
internal/worker/models/loaded_test.go:37:42: discovererFor(srv.URL, nil).Loaded undefined (type *Discoverer has no field or method Loaded)
internal/worker/models/loaded_test.go:73:42: discovererFor(srv.URL, nil).Loaded undefined (type *Discoverer has no field or method Loaded)
internal/worker/models/loaded_test.go:87:44: discovererFor(srv.URL, nil).Loaded undefined (type *Discoverer has no field or method Loaded)
```

- [ ] **Step 3: Write the implementation**

Create `internal/worker/models/loaded.go`:

```go
package models

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Loaded is one model Ollama holds in memory right now (GET /api/ps).
//
// It exists for `memql worker models`, because the question a tuned
// machine raises is not "what is configured" but "did it take": whether
// the model is loaded, whether all of it is on the GPU, at which window,
// and until when. Discovery answers what is INSTALLED; only the runtime can
// answer what is LOADED.
type Loaded struct {
	ID string
	// SizeBytes is the loaded model's total size and VRAMBytes the part of
	// it on the GPU. A model loaded while the card was busy can be mostly
	// on the CPU, which is slow and stays that way until it reloads -- the
	// risk of keeping a model resident on a card other things share.
	SizeBytes, VRAMBytes int64
	// ContextLength is the window the model was loaded with (per slot).
	ContextLength int
	// ExpiresAt is when Ollama will unload it; zero when it did not say.
	ExpiresAt time.Time
}

// foreverHorizon is how far out an expiry must be to be Ollama's spelling
// of keep_alive -1: it reports a date centuries away rather than a flag.
const foreverHorizon = 100 * 365 * 24 * time.Hour

// Forever reports an expiry so far away that it is keep_alive -1.
func (l Loaded) Forever(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && l.ExpiresAt.After(now.Add(foreverHorizon))
}

// GPUPercent is the share of the model on the GPU, 0 to 100. A model whose
// size Ollama did not state reads 0 rather than a guess.
func (l Loaded) GPUPercent() int {
	if l.SizeBytes <= 0 {
		return 0
	}
	p := int(l.VRAMBytes * 100 / l.SizeBytes)
	if p > 100 {
		p = 100
	}
	return p
}

type ollamaPSResponse struct {
	Models []struct {
		Name          string    `json:"name"`
		Model         string    `json:"model"`
		Size          int64     `json:"size"`
		SizeVRAM      int64     `json:"size_vram"`
		ContextLength int       `json:"context_length"`
		ExpiresAt     time.Time `json:"expires_at"`
	} `json:"models"`
}

// Loaded asks the native Ollama what it has in memory. An error is the
// reason it could not be asked, as a sentence the caller can print; an
// empty list with no error is a runtime with nothing loaded, which is the
// ordinary state of a machine nobody has tuned.
func (d *Discoverer) Loaded(ctx context.Context) ([]Loaded, error) {
	var ps ollamaPSResponse
	base := d.ollamaBaseURL()
	if err := d.getJSON(ctx, base+"/api/ps", "", &ps); err != nil {
		return nil, fmt.Errorf("no answer from Ollama's /api/ps at %s (%v)", base, err)
	}
	out := make([]Loaded, 0, len(ps.Models))
	for _, m := range ps.Models {
		id := strings.TrimSpace(m.Model)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		l := Loaded{ID: id, SizeBytes: m.Size, VRAMBytes: m.SizeVRAM, ContextLength: m.ContextLength}
		// Go's zero time is what an absent expires_at decodes to; Ollama
		// also spells "unknown" as year 1.
		if m.ExpiresAt.Year() > 1 {
			l.ExpiresAt = m.ExpiresAt
		}
		out = append(out, l)
	}
	return out, nil
}
```

Create `internal/worker/models_tuning_report.go`:

```go
package worker

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// The tuning half of `memql worker models`: what this machine will tell
// Ollama about keeping models loaded and how many requests to take at once,
// and -- the half that matters -- what Ollama actually holds now. A setting
// is an intention until the runtime shows it kept, and a resident model
// that was loaded while the card was busy is the failure residency makes
// permanent, so the report asks.
//
// Design record: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

// tuningReport is everything printModelTuning prints beyond the inventory.
type tuningReport struct {
	// Parallel and ParallelSource are the per-model ceiling and where it
	// came from (models.Discoverer.ParallelSource).
	Parallel       int
	ParallelSource string
	// Problems are policy.ModelsTuningProblems.
	Problems []string
	// Loaded is what Ollama holds now; LoadedErr is why it could not say.
	Loaded    []models.Loaded
	LoadedErr error
	// Now is the clock, so the report is assertable.
	Now time.Time
}

func printModelTuning(w io.Writer, inv models.Inventory, r tuningReport) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Residency and concurrency")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  Requests per model: %d, from %s.\n", r.Parallel, r.ParallelSource)
	fmt.Fprintln(w, "")

	var kept []models.Info
	for _, m := range inv.Models {
		if m.KeepAlive.Set || m.WorkingContext > 0 {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		fmt.Fprintln(w, "  Kept loaded: nothing is configured to stay loaded. Ollama unloads each model")
		fmt.Fprintln(w, "  five minutes after its last call. Add models.resident to policy.yaml to change")
		fmt.Fprintln(w, "  that, then send the worker a SIGHUP.")
	} else {
		fmt.Fprintln(w, "  Kept loaded (policy.yaml models.resident):")
		for _, m := range kept {
			var parts []string
			if m.KeepAlive.Set {
				parts = append(parts, "kept loaded "+keepAliveWords(m.KeepAlive))
			}
			if m.WorkingContext > 0 {
				parts = append(parts, fmt.Sprintf("at a working context of %d", m.WorkingContext))
			}
			fmt.Fprintf(w, "    %-30s %s\n", m.ID, strings.Join(parts, ", "))
		}
	}
	fmt.Fprintln(w, "")

	switch {
	case r.LoadedErr != nil:
		fmt.Fprintf(w, "  Could not read what Ollama has loaded: %v\n", r.LoadedErr)
	case len(r.Loaded) == 0:
		fmt.Fprintln(w, "  Ollama has nothing loaded now. The next call loads a model, which takes a few")
		fmt.Fprintln(w, "  seconds from a warm disk cache and up to forty from a cold one.")
	default:
		fmt.Fprintln(w, "  Ollama has loaded now:")
		for _, l := range r.Loaded {
			fmt.Fprintf(w, "    %-30s %s\n", l.ID, loadedWords(l, r.Now))
			if l.GPUPercent() < 100 {
				fmt.Fprintln(w, "      The rest runs on the CPU, which is slow, and stays that way until the model reloads.")
				fmt.Fprintf(w, "      Free GPU memory, then run: ollama stop %s\n", l.ID)
			}
		}
	}

	if len(r.Problems) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  Problems with models.parallel and models.resident:")
		for _, p := range r.Problems {
			fmt.Fprintf(w, "    - %s\n", p)
		}
	}
}

func keepAliveWords(k models.KeepAlive) string {
	if k.Forever {
		return "forever"
	}
	return "for " + k.For.String()
}

// loadedWords is one line of what Ollama holds: size, how much of it is on
// the GPU, the window it was loaded at, and when it goes.
func loadedWords(l models.Loaded, now time.Time) string {
	parts := []string{
		fmt.Sprintf("%.1f GB", float64(l.SizeBytes)/1e9),
		fmt.Sprintf("%d%% on the GPU", l.GPUPercent()),
	}
	if l.ContextLength > 0 {
		parts = append(parts, fmt.Sprintf("window %d", l.ContextLength))
	}
	switch {
	case l.ExpiresAt.IsZero():
	case l.Forever(now):
		parts = append(parts, "stays loaded")
	default:
		parts = append(parts, "unloads in "+untilWords(l.ExpiresAt.Sub(now)))
	}
	text := strings.Join(parts, ", ")
	// "35% on the GPU" is a sentence about a fault; the pieces read as a
	// list, so the fault gets its own line (printModelTuning).
	return text
}

// untilWords renders a wait the way an owner thinks of one: hours and
// minutes, or seconds when it is nearly up.
func untilWords(d time.Duration) string {
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		return fmt.Sprintf("%dh%dm", h, int(d.Minutes())-h*60)
	}
}
```

Modify `internal/worker/models_cmd.go`:

```diff
--- a/internal/worker/models_cmd.go
+++ b/internal/worker/models_cmd.go
@@ -74,9 +74,23 @@
 	// to see that they have three models pulled, because "you have models
 	// but this machine cannot serve them" and "you have no models" send
 	// them to entirely different places.
-	inv := newDiscoverer().Probe(ctx, modelsRequest(policy))
+	d := newDiscoverer()
+	inv := d.Probe(ctx, modelsRequest(policy))
 	printModelInventory(os.Stdout, inv, policyPath)
 
+	// The tuning half answers "did it take". It is printed whenever there is
+	// an Ollama to tune or something to say about the policy; a machine with
+	// neither already got "No model runtime answered" above.
+	tuning := policy.ModelsTuning()
+	problems := policy.ModelsTuningProblems()
+	if inv.RuntimeVersions[models.KindOllama] != "" || len(tuning.Resident) > 0 || len(problems) > 0 {
+		n, source := d.ParallelSource(tuning.Parallel)
+		loaded, loadedErr := d.Loaded(ctx)
+		printModelTuning(os.Stdout, inv, tuningReport{
+			Parallel: n, ParallelSource: source, Problems: problems,
+			Loaded: loaded, LoadedErr: loadedErr, Now: time.Now(),
+		})
+	}
 }
 
 // printModelInventory writes the report. Split out so the shape is
```

- [ ] **Step 4: Run them to verify they pass**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ ./internal/worker/ -run 'TestLoaded|TestModelsTuningReport' -count=1 -v
```

Expected: every test PASS (times vary):

```text
--- PASS: TestLoaded (0.00s)
--- PASS: TestLoadedNothingLoadedIsAnAnswer (0.00s)
--- PASS: TestLoadedSaysWhyItCouldNotAsk (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	0.004s
--- PASS: TestModelsTuningReport_SaysWhatIsConfigured (0.00s)
--- PASS: TestModelsTuningReport_NothingConfiguredSaysWhatOllamaDoesInstead (0.00s)
--- PASS: TestModelsTuningReport_SaysWhatOllamaHoldsNow (0.00s)
--- PASS: TestModelsTuningReport_AModelPartlyOnTheCPUIsCalledOut (0.00s)
--- PASS: TestModelsTuningReport_NothingLoadedAndNothingReadable (0.00s)
--- PASS: TestModelsTuningReport_PrintsTheProblems (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	0.017s
```

- [ ] **Step 5: Run the packages, format and vet**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l internal/
go vet ./internal/worker/models/ ./internal/worker/
go test ./internal/worker/models/ ./internal/worker/ -count=1
```

Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the packages pass:

```text
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	<time>
```

- [ ] **Step 6: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add internal/worker/models/loaded.go \
  internal/worker/models/loaded_test.go \
  internal/worker/models_tuning_report.go \
  internal/worker/models_cmd_tuning_test.go \
  internal/worker/models_cmd.go
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "feat(worker): memql worker models reports the tuning and what Ollama holds" \
  -m "The command exists to answer whether a setting took, so it prints the ceiling and its source, what is configured to stay loaded, and what Ollama has in memory with its share on the GPU, window and expiry. A model loaded while the card was busy and left mostly on the CPU is called out." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Documentation

**Files:**
- Modify: `docs/local-models.md`
- Modify: `CLAUDE.md` (`AGENTS.md` is a symlink to it)

**Interfaces:**
- Consumes: every name above.
- Produces: the operator section "Keeping a model loaded" (anchor `#keeping-a-model-loaded`) and the four load-bearing rules in `CLAUDE.md`.

Four exact edits, each an old text that appears once and the text that replaces it. They are the whole of this task.

- [ ] **Step 1: Edit `docs/local-models.md` (three edits)**

Edit 1. Replace:

```markdown
`max_concurrent` is the one attribute that is **never** left absent: the engine
reads a missing ceiling as *unlimited*. `OLLAMA_NUM_PARALLEL` sets it; silence
gets 1.
```

with:

```markdown
`max_concurrent` is the one attribute that is **never** left absent: the engine
reads a missing ceiling as *unlimited*. It is the first of these that gives a
usable number: `models.parallel` in `policy.yaml`; `OLLAMA_NUM_PARALLEL` in the
worker's environment; the `OLLAMA_NUM_PARALLEL` of the `memql-ollama.service`
unit this cockpit manages, drop-ins included; and otherwise 1. `memql worker
models` names which one answered. See [Keeping a model loaded](#keeping-a-model-loaded).
```

Edit 2. Replace:

```markdown
labels — so a model pulled, removed or newly allowed while the worker is
connected is invisible to the cluster until it registers again, and registering
again means reconnecting.
```

with:

```markdown
labels — so a model pulled, removed or newly allowed while the worker is
connected is invisible to the cluster until it registers again, and registering
again means reconnecting. A changed ceiling (`max=`) is a changed label too.
```

Edit 3. Replace:

```markdown
the cache setting. A runtime managed separately keeps its own configuration.

`--model` still overrides, and the closing block still reports what the
**cluster** will see rather than restating the command line.

---

## Measuring a model
```

with:

````markdown
the cache setting. A runtime managed separately keeps its own configuration.
The same goes for how many requests the unit lets Ollama take at once and for
keeping models loaded; both are below.

`--model` still overrides, and the closing block still reports what the
**cluster** will see rather than restating the command line.

---

## Keeping a model loaded

Ollama unloads a model five minutes after its last call and loads it again the
next time anybody asks: about three seconds from a warm disk cache and up to
forty from a cold one, for the 27B the 24 GB class serves. Two things cause a
reload, and a model that is meant to stay has to answer both.

- **The idle timeout.** `keep_alive` moves it.
- **A changed window.** Ollama also reloads a model whenever the `num_ctx` a
  call asks for differs from the one it loaded with, larger or smaller, by a
  single token. The engine rounds each call up to the next power of two, so one
  model is asked for 8192, 16384 and 32768 in turn. On the reference laptop,
  107 of the 170 reloads in thirty days were this and not the timeout.
  `working_context` pins it.

```yaml
models:
  allow:
    - qwen3.8:27b
    - qwen3-embedding:0.6b
  parallel: 2                    # requests per model; see below
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h             # 30m, 2h, 3600 (seconds), or -1 / forever
      working_context: 16384     # every call that fits runs at this window
    - id: qwen3-embedding:0.6b
      keep_alive: 2h
```

A `SIGHUP` applies it; nothing restarts. Delete the block, send another, and
every model is back on Ollama's defaults. Nothing here changes a machine until
its owner writes it.

- **`keep_alive`** is sent on every call to that model: chat, vision, embedding
  and image alike. Ollama applies the value a request states, up or down, and
  what a request that leaves it out does varies by release, so no route leaves
  it out. `0` is refused (Ollama reads it as "unload after every call"), and so
  is anything past 30 days; `-1` keeps the model loaded until Ollama stops. A
  resident model is not pinned: Ollama still evicts an idle one when another must
  load and does not fit, so a resident 9B is displaced by a call for the 27B
  unless the 27B is not in `models.allow`.
- **`working_context`** is a floor on `num_ctx`: a call that needs no more runs
  at exactly it, and a call that needs more runs at what it asked for. It is the
  window per slot, and the model's cache is sized for the slots times it, so it is
  also the memory knob. Embeddings always run at 8192 and ignore it.
- Neither is a label attribute. The engine is told nothing about them, and the
  advertisement changes only in `max=`.

`memql worker models` prints what is configured, then asks Ollama what it holds:
the size, how much of it is on the GPU, the window it was loaded at, and when it
goes. A model that is mostly on the CPU is called out, because residency makes
that permanent: a model loaded while another process held the card stays where
it was put until it reloads. On a card shared with another Ollama or a GPU
service, account for the other tenant's memory before making any model resident.

### Requests at once

`parallel` is the ceiling the worker advertises and enforces for each model, and
Ollama has to be given the same number or the extra calls queue inside it. For
the unit this cockpit installs, the unit is the source: a fresh install on a 24 or
32 GB machine writes `Environment=OLLAMA_NUM_PARALLEL=2`, and the worker reads it
back, drop-ins included. For an existing unit add the line yourself (a drop-in,
`systemctl --user edit memql-ollama.service`, is the way systemd documents it),
then `systemctl --user daemon-reload` and `systemctl --user restart
memql-ollama.service`. The restart cancels what Ollama is generating and unloads
its models, so do it between calls. `models.parallel` overrides the unit for a
runtime the cockpit cannot read: Docker, or macOS.

Every slot costs memory: a constant for the model's recurrent state plus its
cache for the window. For `qwen3.8:27b` (Q4_K_M, q8_0 cache) on a 24 GB RTX 5090
Laptop, llama-server's own report in `~/.memql/state/ollama.log` gives the first
slot (`projected to use`) and what each further slot adds: 748 MiB of recurrent
state plus 38 KiB of cache per token of window. In MiB, with the card's 22,966
free and the report wanting at least 1,936 left:

| slots x window | 27B | with the embedder | left |
|---|---|---|---|
| 1 x 32K | 17,673 | 19,073 | 3,893 |
| 2 x 8K | 17,963 | 19,863 | 3,103 |
| 2 x 16K | 18,325 | 20,225 | 2,741 |
| 2 x 32K | 19,637 | 21,537 | 1,429 |
| 3 x 16K | 19,681 | 22,081 | 885 |

Only the one-slot figure is read directly; the rest add the per-slot costs above.
The embedder column is estimated from its architecture (it was never loaded in the
thirty days measured). The report the runtime prints on every load says whether a
choice held: `no changes needed`, and `offloaded N/N layers to GPU`.

---

## Measuring a model
````

- [ ] **Step 2: Edit `CLAUDE.md` (one edit)**

Replace:

```markdown
The engine parses these model attributes. `TestWireContract` checks the
shared names against the pinned engine; keep both sides synchronized.
```

with:

```markdown
The engine parses these model attributes. `TestWireContract` checks the
shared names against the pinned engine; keep both sides synchronized.

**Residency and slots** (`models.resident`, `models.parallel`; spec
`docs/superpowers/specs/2026-10-18-fleet-tuning-design.md`, operator doc
[docs/local-models.md](docs/local-models.md)). Four rules, each of which fails
silently:

1. **`keep_alive` rides every route that sends a model work** -- chat, vision
   (a chat turn), embeddings, image generation (`ollamaClient.stampKeepAlive`;
   `TestOllamaKeepAlive_EveryGenerationRouteCarriesIt` lists the routes). Ollama
   applies the keep_alive a request STATES, up or down, and what an omission does
   varies by release. Forever is the JSON number `-1`, never the string `"-1"`,
   which Ollama refuses. Not to be confused with `keepalive`, the liveness delta
   a call sends the cluster.
2. **`keep_alive` alone does not make a model resident; the window does.** Ollama
   reloads on ANY change of `num_ctx`, and the engine rounds each call up to a
   power of two (`contextStartForCandidate`). `working_context` is a FLOOR:
   `ollamaClient.window` never sends less, and never clamps a larger call down.
3. **The settings ride `models.Info`, never the label.** Only `max=` changes, so
   `TestWireContract` is untouched. `newDiscoverer` and `modelsRequest` are the
   only places a `Discoverer` or a `Request` is built
   (`TestEveryDiscoveryIsBuiltInOnePlace`); a command that built its own would
   print a ceiling the worker does not advertise.
4. **The managed unit is the source of the slot count.** `max=` is
   `models.parallel`, else the worker's `OLLAMA_NUM_PARALLEL`, else the unit's
   (`inference.UnitParallel`, drop-ins included), else 1 -- never 0, which the
   engine reads as unlimited. Setup writes the unit once and never touches a
   runtime that is already serving, so an existing unit is edited by hand.

There is no warm-up and no per-level slot reservation, on purpose: a warm-up can
load a model onto a GPU another process holds, and residency then makes that
permanent; the slot-wait log line (`model call waited for a concurrency slot`) is
the evidence for reserving slots (spec, D4 and D6).
```

- [ ] **Step 3: Check the anchors and that the symlink survived**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
grep -n '^## Keeping a model loaded\|^### Requests at once' docs/local-models.md
grep -n 'keeping-a-model-loaded' docs/local-models.md
grep -n 'Residency and slots' CLAUDE.md
ls -l AGENTS.md
```

Expected: both headings, the link and the `CLAUDE.md` paragraph each appear once, and `AGENTS.md` is still a symlink:

```text
573:## Keeping a model loaded
628:### Requests at once
254:models` names which one answered. See [Keeping a model loaded](#keeping-a-model-loaded).
408:**Residency and slots** (`models.resident`, `models.parallel`; spec
AGENTS.md -> CLAUDE.md
```

- [ ] **Step 4: Commit**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git add docs/local-models.md \
  CLAUDE.md
git -c user.name=znas -c user.email=znas@znas.io commit \
  -m "docs: keeping a model loaded and the slot count" \
  -m "The operator section explains the two causes of a reload, the policy keys, how to change an existing unit, and the measured memory per slot. CLAUDE.md records the four rules that fail silently: every route states keep_alive, the window and not keep_alive stops reloads, settings ride Info and not the label, and the unit is the source of the slot count." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Whole-branch verification

**Files:** none.

**Interfaces:**
- Consumes: Tasks 1-10.
- Produces: a branch that passes what CI runs (`.github/workflows/ci.yml`: `go build ./...`, `go test -race -count=1 -timeout=600s ./...`, `gofmt -l .`, `go vet ./...`, the fuzz targets for 20 s each) and touches only the files in the File Structure table.

- [ ] **Step 1: Format, vet, build**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
gofmt -l .
go vet ./...
go build ./...
```

Expected:

```text
(no output)
```

- [ ] **Step 2: Run the whole suite under the race detector, as CI does (a few minutes; modelcall and tools dominate)**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test -race -count=1 -timeout=600s ./...
```

Expected: every package `ok`; none FAIL (`scripts/macos` reads the owner's own `../memql/scripts/lib/capability.sh`, which exists):

```text
ok  	github.com/znasllc-io/memql-cockpit/cmd/memql	<time>
...
ok  	github.com/znasllc-io/memql-cockpit/internal/worker	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/inference	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/modelcall	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/tools	<time>
...
ok  	github.com/znasllc-io/memql-cockpit/scripts/macos	<time>
```

- [ ] **Step 3: Fuzz the two new targets for the length CI gives them**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run '^$' -fuzz '^FuzzParseKeepAlive$' -fuzztime=20s
go test ./internal/worker/inference/ -run '^$' -fuzz '^FuzzUnitParallel$' -fuzztime=20s
```

Expected: PASS for both, and no `testdata/fuzz` directory appears (a crasher would write one):

```text
fuzz: elapsed: 20s, execs: <n> (<n>/sec), new interesting: <n> (total: <n>)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
...
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/inference	<time>
```

- [ ] **Step 4: Check the wire contract and the touched-file list**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
go test ./internal/worker/models/ -run TestWireContract -v -count=1 | tail -3
git status --short
git diff --name-only origin/main...HEAD -- . ':!docs' | sort
```

Expected: `TestWireContract` passes, `git status` prints nothing, and the branch touches exactly these files outside `docs/` (no `go.mod`, `go.sum`, `.github`, scripts or generated files):

```text
--- PASS: TestWireContract (0.00s)
PASS
ok  	github.com/znasllc-io/memql-cockpit/internal/worker/models	<time>
CLAUDE.md
internal/worker/cli.go
internal/worker/inference/fuzz_test.go
internal/worker/inference/parallel.go
internal/worker/inference/parallel_test.go
internal/worker/inference/plan.go
internal/worker/inference/stage.go
internal/worker/inference_cmd.go
internal/worker/modelcall/modalities.go
internal/worker/modelcall/ollama.go
internal/worker/modelcall/ollama_resident_test.go
internal/worker/modelcall/session.go
internal/worker/modelcall/slotwait_test.go
internal/worker/modelinventory.go
internal/worker/models/discover.go
internal/worker/models/discover_test.go
internal/worker/models/fuzz_test.go
internal/worker/models/loaded.go
internal/worker/models/loaded_test.go
internal/worker/models/models.go
internal/worker/models/ollama.go
internal/worker/models/resident.go
internal/worker/models/resident_discovery_test.go
internal/worker/models/resident_test.go
internal/worker/models_cmd.go
internal/worker/models_cmd_tuning_test.go
internal/worker/models_tuning_report.go
internal/worker/models_tuning_wiring_test.go
internal/worker/pair.go
internal/worker/probe_cmd.go
internal/worker/runtime_cmd.go
internal/worker/tools/models_tuning.go
internal/worker/tools/models_tuning_policy_test.go
internal/worker/tools/policy.go
```

- [ ] **Step 5: Only when the owner says so: delete this plan (the repository's convention) and open the PR**

```bash
cd /home/znas/memql-projects/memql-cockpit-fleet
git rm docs/superpowers/plans/2026-10-18-fleet-tuning.md
git -c user.name=znas -c user.email=znas@znas.io commit -m "docs(plans): delete the fleet-tuning plan -- its work ships in this PR" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push -u origin feat/fleet-keepalive
gh pr create --title "feat(worker): keep a model resident and take requests in parallel" --body-file - <<'EOF'
Spec: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md

models.resident and models.parallel in policy.yaml; keep_alive on every Ollama route; a working window so power-of-two num_ctx changes stop reloading the model; the managed unit as the source of the slot count; memql worker models shows what Ollama holds.

Nothing changes on a machine until its owner writes the keys.
EOF
```

The runbook tasks that follow (12 and 13) are local operations on the owner's laptop. If the PR is merged first, the same commands live in the spec (section 8) and the operator doc, and the plan can go with the PR.

---

### Task 12: Local rollout, Stage A -- install the branch build (one worker restart)

**Files (outside the repo):** `/home/znas/.local/state/memql/fleet-tuning/load-census.py`, `/home/znas/.local/state/memql/fleet-tuning/before/*`, `/home/znas/.local/state/memql/fleet-tuning/memql-computeruse-linux-amd64`, `~/.memql/bin/memql-computeruse-linux-amd64`.

**Interfaces:**
- Consumes: a green, committed branch (Task 11); the pinned environment (Task 1).
- Produces: the owner's worker running the branch build with nothing configured. It behaves exactly as before: `memql worker models` says `Requests per Ollama model: 1, from the default.` and `nothing is configured to stay loaded`.

The owner granted permission to run branch builds locally. Steps marked **DISRUPTIVE** interrupt the owner's other memQL work; each says what and how to check first. **What this machine runs today**, read on 2026-10-18: `memql-worker.service` runs `~/.memql/bin/memql worker run`, a symlink to `memql-computeruse-linux-amd64`, version `0.20.0+g58ad26323197 (computeruse)`, connected to two homes (`api.memql.znas.io` and `api.memql.localhost`) and allowed to run CI pipeline steps (`pipelines.allow: true`); `memql-ollama.service` runs Ollama 0.34.2 (written by `Stage.Unit()`, no `NUM_PARALLEL`, no `KEEP_ALIVE`); `memql-menu.service` is the tray companion. The worker has no drain: a restart cancels everything in flight on both homes.

- [ ] **Step 1: Capture the before state (read-only)**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
mkdir -p "$FT/before"
~/.memql/bin/memql --version | tee "$FT/before/version.txt"
cp -p ~/.memql/policy.yaml "$FT/before/policy.yaml"
systemctl --user cat memql-ollama.service > "$FT/before/memql-ollama.service"
cp -p ~/.memql/bin/memql-computeruse-linux-amd64 "$FT/before/memql-computeruse-linux-amd64"
```

Expected:

```text
memql 0.20.0+g58ad26323197 (computeruse)
```

- [ ] **Step 2: Save the census script and take the baseline (read-only). It counts model loads in the fleet Ollama's log and says why each happened; the same command with a date reads only the lines since then**

Create `/home/znas/.local/state/memql/fleet-tuning/load-census.py`:

```python
#!/usr/bin/env python3
"""Count model loads in the fleet Ollama's log and say why each one happened.

usage: python3 -I load-census.py [LOG] [SINCE]
  LOG    defaults to ~/.memql/state/ollama.log
  SINCE  an ISO date or time (2026-10-19 or 2026-10-19T08:00:00); earlier
         lines are ignored, so the same file can be read before and after a
         change.

A reload is a line the runtime prints when it starts llama-server for a
model. Two causes are told apart, because they have different cures:
  window changed  the requested num_ctx differs from the last load of the
                  same model (the engine rounds each call up to a power of
                  two). The cure is models.resident working_context.
  same window     everything else: the keep_alive expired, another model
                  needed the space, or Ollama restarted. The cure for the
                  first is models.resident keep_alive.
"""
import datetime as dt
import json
import os
import re
import statistics
import sys
from collections import Counter, defaultdict

home = os.path.expanduser("~")
log_path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(home, ".memql/state/ollama.log")
since = None
if len(sys.argv) > 2:
    since = dt.datetime.fromisoformat(sys.argv[2])


def blob_names():
    """digest prefix -> model name, from the manifests Ollama keeps."""
    names = {}
    root = os.path.join(home, ".memql/ollama/models/manifests")
    for base, _, files in os.walk(root):
        for f in files:
            path = os.path.join(base, f)
            rel = os.path.relpath(path, root).split(os.sep)  # host/namespace/model/tag
            try:
                manifest = json.load(open(path))
            except (OSError, ValueError):
                continue
            for layer in manifest.get("layers", []):
                if layer.get("mediaType", "").endswith("image.model"):
                    names[layer["digest"].split(":")[-1][:8]] = "%s:%s" % (rel[-2], rel[-1])
    return names


rx_load = re.compile(r'^time=(\S+) .*msg="starting llama-server" cmd="[^"]*?--model \S*sha256-(\w{8})\w* .*? -c (\d+) -np (\d+)')
rx_done = re.compile(r'^time=\S+ .*msg="llama-server started in ([\d.]+) seconds"')
rx_chat = re.compile(r'^\[GIN\] (\d{4})/(\d\d)/(\d\d) - (\d\d):(\d\d):(\d\d) \| \d+ \|[^|]*\|[^|]*\| POST\s+"/api/(chat|embed|generate)"')

names = blob_names()
loads = []  # [timestamp, blob, ctx, np, seconds]
requests = Counter()
pending = None
with open(log_path, errors="replace") as f:
    for line in f:
        m = rx_load.match(line)
        if m:
            ts = dt.datetime.fromisoformat(m.group(1)).replace(tzinfo=None)
            if since and ts < since:
                pending = None
                continue
            pending = [ts, m.group(2), int(m.group(3)), int(m.group(4)), None]
            loads.append(pending)
            continue
        m = rx_done.match(line)
        if m and pending is not None and pending[4] is None:
            pending[4] = float(m.group(1))
            continue
        m = rx_chat.match(line)
        if m:
            ts = dt.datetime(*(int(m.group(i)) for i in range(1, 7)))
            if not since or ts >= since:
                requests[m.group(7)] += 1

if not loads:
    print("no model loads in %s%s" % (log_path, " since %s" % since if since else ""))
    print("requests: %s" % dict(requests))
    sys.exit(0)

span_days = max(1.0, (loads[-1][0] - loads[0][0]).total_seconds() / 86400)
print("log: %s" % log_path)
print("from %s to %s (%.1f days)" % (loads[0][0], loads[-1][0], span_days))
print("requests: %s" % dict(requests))
print("loads: %d (%.1f a day)" % (len(loads), len(loads) / span_days))

by_model = defaultdict(list)
for row in loads:
    by_model[row[1]].append(row)

for blob, rows in sorted(by_model.items(), key=lambda kv: -len(kv[1])):
    print()
    print("%s  (blob %s): %d loads" % (names.get(blob, "unknown model"), blob, len(rows)))
    # llama-server's -c is the window of every slot together (num_ctx x slots), so
    # the window one call asked for is -c divided by -np.
    windows = Counter((r[2] // max(r[3], 1), r[3]) for r in rows)
    print("  windows (num_ctx x slots): %s" % ", ".join("%d x%d: %d" % (w, np, n) for (w, np), n in sorted(windows.items())))
    cause = Counter()
    for prev, cur in zip(rows, rows[1:]):
        cause["window changed" if prev[2] // max(prev[3], 1) != cur[2] // max(cur[3], 1) else "same window"] += 1
    if cause:
        print("  after the first load: %s" % ", ".join("%s %d" % (k, v) for k, v in sorted(cause.items())))
    secs = sorted(r[4] for r in rows if r[4] is not None)
    if secs:
        p95 = secs[min(len(secs) - 1, int(round(0.95 * (len(secs) - 1))))]
        print("  load time: median %.1fs, p95 %.1fs, max %.1fs, over 10s: %d" % (statistics.median(secs), p95, secs[-1], sum(1 for s in secs if s > 10)))

perday = Counter(r[0].date().isoformat() for r in loads)
print()
print("loads per day (last 10 days with any): %s" % ", ".join("%s %d" % (d, n) for d, n in sorted(perday.items())[-10:]))
```

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
python3 -I "$FT/load-census.py" | tee "$FT/before/census.txt"
```

Expected on 2026-10-18 (later runs add lines; the first four numbers only grow):

```text
log: /home/znas/.memql/state/ollama.log
from 2026-09-18 00:15:43.087000 to 2026-10-18 05:56:55.637000 (30.2 days)
requests: {'chat': 666}
loads: 171 (5.7 a day)

qwen3.8:27b  (blob f5f1dd89): 171 loads
  windows (num_ctx x slots): 8192 x1: 78, 16384 x1: 59, 32768 x1: 34
  after the first load: same window 63, window changed 107
  load time: median 2.9s, p95 3.8s, max 41.7s, over 10s: 5
```

- [ ] **Step 3: Smoke the new code against the live Ollama with a headless build (read-only; it connects to no cluster)**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
REV=$(git rev-parse --short=12 HEAD)
DIRTY=$(test -z "$(git status --porcelain --untracked-files=normal)" || echo true)
CGO_ENABLED=0 go build -ldflags "-X main.version=$(cat VERSION) -X main.revision=$REV -X main.dirty=$DIRTY" -o "$FT/memql-headless-smoke" ./cmd/memql
"$FT/memql-headless-smoke" --version
"$FT/memql-headless-smoke" worker models | sed -n '/Residency and concurrency/,$p'
```

Expected (the headless build is for this check only; it must never replace the installed computer-use build):

```text
memql 0.20.0+g<REV> (headless)

Residency and concurrency

  Requests per Ollama model: 1, from the default.

  Kept loaded: nothing is configured to stay loaded. By default, Ollama unloads
  each model five minutes after its last call. Add models.resident to
  policy.yaml to change that, then send the worker a SIGHUP.

  Ollama has nothing loaded now. The next call loads a model, which takes a few
  seconds from a warm disk cache and up to forty from a cold one.
```

- [ ] **Step 4: Check the computer-use build's dependencies. THE OWNER RUNS the install line (the cockpit runs no sudo); it adds development files only and affects nothing running**

```bash
for h in X11/Xutil.h X11/extensions/XTest.h X11/extensions/Xinerama.h xkbcommon/xkbcommon.h png.h; do
  test -e /usr/include/$h && echo "have    $h" || echo "MISSING $h"
done
# the owner, once (the list release.yml and ci.yml use):
#   sudo apt-get install -y libxtst-dev libxinerama-dev libxkbcommon-dev libxkbcommon-x11-dev libpng-dev libx11-dev libxi-dev
```

Expected on 2026-10-18, before the owner installs them (all `have` afterwards). Without them the build in the next step stops at `fatal error: X11/Xutil.h: No such file or directory`. Stop here until they are installed: replacing the computer-use build with a headless one would drop a capability this machine advertises.

```text
MISSING X11/Xutil.h
MISSING X11/extensions/XTest.h
MISSING X11/extensions/Xinerama.h
MISSING xkbcommon/xkbcommon.h
MISSING png.h
```

- [ ] **Step 5: Build the computer-use variant, the way `make cockpit-computeruse-host` does, against the pinned engine**

```bash
source /home/znas/.local/state/memql/fleet-tuning/env.sh
cd /home/znas/memql-projects/memql-cockpit-fleet
REV=$(git rev-parse --short=12 HEAD)
DIRTY=$(test -z "$(git status --porcelain --untracked-files=normal)" || echo true)
CGO_ENABLED=1 go build -tags computeruse -ldflags "-X main.version=$(cat VERSION) -X main.revision=$REV -X main.dirty=$DIRTY" -o "$FT/memql-computeruse-linux-amd64" ./cmd/memql
"$FT/memql-computeruse-linux-amd64" --version
"$FT/memql-computeruse-linux-amd64" worker models | sed -n '/Requests per Ollama model/p'
```

Expected (a `.dirty` suffix means uncommitted changes: commit first):

```text
memql 0.20.0+g<REV> (computeruse)
  Requests per Ollama model: 1, from the default.
```

- [ ] **Step 6: DISRUPTIVE pre-flight: is the worker idle? Anything in its cgroup besides the main process is work in flight (an app session, a pipeline step, a shell command)**

```bash
systemctl --user status memql-worker.service --no-pager | sed -n '/CGroup/,$p'
docker ps --format 'table {{.Names}}\t{{.Status}}'
tail -n 2 ~/.memql/state/ollama.log | cut -c1-110
```

Expected when idle: exactly one process under the unit, no recently started CI or workbench container in the docker list, and no `/api/chat` line within the last minute. If not, wait. The run it interrupts is lost on both homes.

```text
     CGroup: /user.slice/user-1000.slice/user@1000.service/app.slice/memql-worker.service
             └─3086619 /home/znas/.memql/bin/memql worker run
```

- [ ] **Step 7: DISRUPTIVE: swap the binary (a rename; the running process keeps the old file) and restart the worker. About ten seconds on both homes**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
install -m 0755 "$FT/memql-computeruse-linux-amd64" ~/.memql/bin/memql-computeruse-linux-amd64.new
mv -f ~/.memql/bin/memql-computeruse-linux-amd64.new ~/.memql/bin/memql-computeruse-linux-amd64
systemctl --user restart memql-worker.service
for i in $(seq 1 30); do
  ~/.memql/bin/memql worker control | python3 -I -c 'import sys,json; s=json.load(sys.stdin)["status"]; print(s["version"], [(h["id"], h["state"]) for h in s["homes"]])' | grep -q "Connected.*Connected" && break
  sleep 2
done
~/.memql/bin/memql worker control | python3 -I -c 'import sys,json; s=json.load(sys.stdin)["status"]; print(s["version"], [(h["id"], h["state"]) for h in s["homes"]])'
```

Expected:

```text
0.20.0+g<REV> [('api.memql.localhost', 'Connected'), ('api.memql.znas.io', 'Connected')]
```

- [ ] **Step 8: Verify the restart took and nothing regressed**

```bash
~/.memql/bin/memql --version
grep '"msg":"worker registered with cluster"' ~/.memql/state/worker.log | tail -2 | cut -c1-230
curl -s http://127.0.0.1:9100/metrics | grep '^worker_home_connected'
~/.memql/bin/memql worker models | grep -E 'machine concurrency|Requests per Ollama model'
grep '"msg":"model concurrency ceiling"' ~/.memql/state/worker.log | tail -2 | cut -c1-230
```

Expected:

```text
memql 0.20.0+g<REV> (computeruse)
{"time":"...","level":"INFO","msg":"worker registered with cluster","home":"api.memql.localhost",...,"models_offered":2,...}
{"time":"...","level":"INFO","msg":"worker registered with cluster","home":"api.memql.znas.io",...,"models_offered":2,...}
worker_home_connected{home="api.memql.localhost"} 1
worker_home_connected{home="api.memql.znas.io"} 1
  capability MODEL, machine concurrency 2
  Requests per Ollama model: 1, from the default.
{"time":"...","level":"INFO","msg":"model concurrency ceiling","model":"qwen3-embedding:0.6b","model_max":1,"source":"the default"}
{"time":"...","level":"INFO","msg":"model concurrency ceiling","model":"qwen3.8:27b","model_max":1,"source":"the default"}
```

The last two lines are the worker's own record of the ceiling it advertises, written at start and again after every `SIGHUP`. `memql worker models` is run from a shell, so it reads the worker's environment file (`~/.memql/worker.env`, as it is now) rather than the worker's own environment; these lines are what the worker really used.

- [ ] **Step 9: Optional: restart the tray companion so it runs the new binary too (it only shows status)**

```bash
systemctl --user restart memql-menu.service
```

- [ ] **Step 10: ROLLBACK, if anything above is wrong (also DISRUPTIVE; same pre-flight)**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
install -m 0755 "$FT/before/memql-computeruse-linux-amd64" ~/.memql/bin/memql-computeruse-linux-amd64.new
mv -f ~/.memql/bin/memql-computeruse-linux-amd64.new ~/.memql/bin/memql-computeruse-linux-amd64
systemctl --user restart memql-worker.service
~/.memql/bin/memql --version
```

Expected:

```text
memql 0.20.0+g58ad26323197 (computeruse)
```

---

### Task 13: Local rollout, Stage B -- make the bake-off's winner resident

**Files (outside the repo):** `~/.memql/policy.yaml`, `~/.config/systemd/user/memql-ollama.service.d/10-fleet-tuning.conf`, `/home/znas/.local/state/memql/fleet-tuning/after/*`.

**Interfaces:**
- Consumes: Stage A installed (Task 12); S1 Task 22's recorded winner; a card with room (Step 1).
- Produces: the winner and the embedder resident, `max=` equal to Ollama's slots, measurements showing it took, and the 2 x 32K load checked against the runtime's own margin (Step 8).

Do not start before the **spec section 6** question is answered: which Ollama owns the card in this phase. Today `rmf-ollama` holds `qwen2.5:14b-instruct` (15.7 GB, `OLLAMA_KEEP_ALIVE=-1`) and `rmf-whisper` holds about 1.5 GB; the fleet's Ollama has nothing loaded and 6,496 MiB free. A warm-up now would load most of the 27B onto the CPU and `keep_alive` would keep it there.

**Bake-off alignment (S1 Task 22, spec section 7).** Run each candidate at the profile it would be made resident at (`numCtx` equal to the planned working window, the slots the profile gives), warm each model first and drop that call, and run the 27B only with the card free. Then apply the winner:

| Winner | Pull and allow | `resident` | `parallel` |
|---|---|---|---|
| `qwen3.8:27b` (already on the fleet) | none | 27B and embedder, `2h`, window 16384 | 2 on a free card, 1 with whisper still resident (Step 1) |
| `qwen3.5:9b` | `memql worker models --pull qwen3.5:9b --allow qwen3.5:9b`; take the 27B out of `models.allow` if the 9B must stay (Ollama evicts an idle model when another must load) | the 9B and embedder | read the first slot and the per-slot cost from the load report (Step 6), then the largest count that leaves the margin |
| `qwen2.5:14b-instruct` | pull and allow it into the fleet's Ollama (it is RMF's today) | that model and the embedder | as above |

`resident[].id` is the id S3 pins in `@defaultProvider("fleet:<id>")`, byte for byte; it must appear as a `model:<id>` label in `memql worker models`.

- [ ] **Step 1: Read the card (read-only) and pick the profile**

```bash
nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv,noheader
nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv,noheader
curl -s http://127.0.0.1:11435/api/ps | python3 -I -c 'import sys,json; [print("rmf-ollama holds", m["model"], m["size_vram"]//2**20, "MiB") for m in json.load(sys.stdin)["models"]]'
```

Read on 2026-10-18, and the answer is to stop: 6,496 MiB free. The profile that holds depends on what else stays on the card (spec section 5; the margin is the 1,936 MiB the runtime's own report asks for):

```text
24463 MiB, 17486 MiB, 6496 MiB
3856498, /usr/lib/ollama/llama-server, 15324 MiB
3857783, /usr/lib/ollama/llama-server, 608 MiB
3897029, /app/.venv/bin/python, 1534 MiB
rmf-ollama holds qwen2.5:14b-instruct <MiB> MiB
rmf-ollama holds nomic-embed-text:latest <MiB> MiB
```

| What else is on the card | Profile that keeps the margin | MiB that must be free at warm-up |
|---|---|---|
| nothing | 2 slots x 16K (`parallel: 2`, `working_context: 16384`) | 22,161 |
| whisper (about 1,536 MiB) | 1 slot x 32K (`parallel: 1`, `working_context: 32768`) | 21,009 |
| RMF's 14B | none: do not make the 27B resident | |

**DISRUPTIVE to the RMF stack, which is the pals' brain until S3:** `docker stop rmf-ollama` (and `rmf-whisper` if the profile needs it) frees the card. Do it when nobody is playing, and only after the pals decide through the fleet (S3) or the owner has chosen to stop RMF's Ollama; with `OLLAMA_KEEP_ALIVE=-1`, merely unloading its model lets the next RMF call load it again.

- [ ] **Step 2: Set the slot count where Ollama reads it: a drop-in (nothing happens until Ollama restarts, but the worker advertises it at its next refresh, at most 90 s later)**

```bash
mkdir -p ~/.config/systemd/user/memql-ollama.service.d
cat > ~/.config/systemd/user/memql-ollama.service.d/10-fleet-tuning.conf <<'EOF'
[Service]
# Requests a model takes at once. The worker reads this and advertises the same ceiling.
Environment=OLLAMA_NUM_PARALLEL=2
# Optional safety net: a client that does not state keep_alive gets this instead of five minutes.
# Environment=OLLAMA_KEEP_ALIVE=2h
EOF
systemctl --user daemon-reload
systemctl --user cat memql-ollama.service | tail -6
~/.memql/bin/memql worker models | grep -E 'Requests per Ollama model|model:qwen3.8:27b='
```

Expected (the worker re-registers both homes once, when idle):

```text
# /home/znas/.config/systemd/user/memql-ollama.service.d/10-fleet-tuning.conf
[Service]
# Requests a model takes at once. The worker reads this and advertises the same ceiling.
Environment=OLLAMA_NUM_PARALLEL=2
# Optional safety net: a client that does not state keep_alive gets this instead of five minutes.
# Environment=OLLAMA_KEEP_ALIVE=2h
  Requests per Ollama model: 2, from the memql-ollama.service unit.
  model:qwen3.8:27b=ctx=262144,structured=1,max=2,params=27320697856,quant=Q4_K_M,tools=1,vision=1
```

- [ ] **Step 3: DISRUPTIVE: restart Ollama so it takes two slots. This cancels what it is generating and unloads its models; a few seconds. Check nothing is mid-generation first (`curl -s http://127.0.0.1:11434/api/ps`; a model with an expiry a few minutes out was used a moment ago)**

```bash
systemctl --user restart memql-ollama.service
curl -s http://127.0.0.1:11434/api/version
grep 'server config' ~/.memql/state/ollama.log | tail -1 | grep -o 'OLLAMA_NUM_PARALLEL:[0-9]*\|OLLAMA_KEEP_ALIVE:[0-9a-z]*'
```

Expected (`OLLAMA_KEEP_ALIVE:2h0m0s` if the optional line was enabled):

```text
{"version":"0.34.2"}
OLLAMA_KEEP_ALIVE:5m0s
OLLAMA_NUM_PARALLEL:2
```

- [ ] **Step 4: Write the residency policy (the winner is the 27B here) and apply it with a SIGHUP; nothing restarts**

Add under the existing `models:` block of `~/.memql/policy.yaml`, keeping `allow` as it is:

```yaml
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
    - id: qwen3-embedding:0.6b
      keep_alive: 2h
```

```bash
kill -HUP "$(systemctl --user show memql-worker.service -p MainPID --value)"
sleep 3   # the worker looks at Ollama again after the reload, and logs the ceilings a moment later
grep -E '"msg":"policy reloaded"|policy.yaml models has a problem|"msg":"model concurrency ceiling"' ~/.memql/state/worker.log | tail -3 | cut -c1-230
~/.memql/bin/memql worker models | sed -n '/Kept loaded/,/^$/p'
```

Expected: the reload line, the two ceiling lines the reload writes (`model_max` 2 from the unit, or from `models.parallel` if the policy sets it; the worker looks again after the reload), no `has a problem` line, and the two entries:

```text
{"time":"...","level":"INFO","msg":"policy reloaded"}
{"time":"...","level":"INFO","msg":"model concurrency ceiling","model":"qwen3-embedding:0.6b","model_max":2,"source":"the memql-ollama.service unit"}
{"time":"...","level":"INFO","msg":"model concurrency ceiling","model":"qwen3.8:27b","model_max":2,"source":"the memql-ollama.service unit"}
  Kept loaded (policy.yaml models.resident):
    qwen3.8:27b                    kept loaded for 2h0m0s, at a working context of 16384
    qwen3-embedding:0.6b           kept loaded for 2h0m0s
```

- [ ] **Step 5: Warm both models with the exact options the cockpit will send (it sends `num_ctx` equal to the window; the embedder always uses 8192 and, for this one, a batch of 512), so the first real call finds the runner. THIS PUTS ABOUT 20 GB ON THE CARD**

```bash
curl -s http://127.0.0.1:11434/api/generate -d '{"model":"qwen3.8:27b","keep_alive":"2h","options":{"num_ctx":16384}}'
echo
curl -s http://127.0.0.1:11434/api/embed -d '{"model":"qwen3-embedding:0.6b","input":["warm"],"keep_alive":"2h","truncate":false,"options":{"num_ctx":8192,"num_batch":512}}' -o /dev/null -w 'embed http=%{http_code}\n'
```

Expected:

```text
{"model":"qwen3.8:27b","created_at":"...","response":"","done":true,"done_reason":"load"}
embed http=200
```

- [ ] **Step 6: Verify it took: loaded, fully on the GPU, at the window, expiring about two hours out; and the runtime's own report says the choice held**

```bash
~/.memql/bin/memql worker models | sed -n '/Ollama has loaded now/,/^$/p'
grep -E 'projected to use|will leave|offloaded' ~/.memql/state/ollama.log | tail -4 | cut -c1-170
nvidia-smi --query-gpu=memory.used,memory.free --format=csv,noheader
```

Expected (sizes approximate; both loaded; every line below must hold). If the 27B is under 100% on the GPU, if `will leave` is below its stated target, or if only one of the two models is loaded, go to the rollback and use the smaller profile:

```text
  Ollama has loaded now:
    qwen3.8:27b                    18.4 GB, 100% on the GPU, window 16384, unloads in 1h59m
    qwen3-embedding:0.6b           1.4 GB, 100% on the GPU, window 8192, unloads in 1h59m
... common_params_fit_impl: projected to use <N> MiB of device memory vs. <M> MiB of free device memory
... common_params_fit_impl: will leave <K> >= 1936 MiB of free device memory, no changes needed
... load_tensors: offloaded 66/66 layers to GPU
<used> MiB, <free> MiB
```

- [ ] **Step 7: Soak, then measure. After a play session or a day; put the date of Step 5 where 2026-10-19 is**

```bash
FT=/home/znas/.local/state/memql/fleet-tuning
mkdir -p "$FT/after"
python3 -I "$FT/load-census.py" ~/.memql/state/ollama.log 2026-10-19 | tee "$FT/after/census.txt"
grep -c 'waited for a concurrency slot' ~/.memql/state/worker.log
grep -c 'refused after waiting for a concurrency slot' ~/.memql/state/worker.log
```

Acceptance (spec section 8, restated by the final review): no window-change reload at or below 16K; anything else at most one or two a day (baseline 5.7 a day, 63% window changes); no refusal after waiting for a fast-level call. A call the engine sizes at 32K is a different window and reloads the 27B once, and the next call at 16K reloads it back: that is by design with `working_context: 16384`, and Step 8 checks what it costs with two slots. The wait counts are the evidence for spec D6 (per-level reservation); record them.

```text
qwen3.8:27b  (blob f5f1dd89): <few> loads
  windows (num_ctx x slots): 16384 x2: <few>
  after the first load: same window <few>      <- 'window changed' absent or near zero
<count of waits of a second or more>
0                                                <- refusals after waiting
```

- [ ] **Step 8: Check the 2 x 32K case with the embedder resident. The profile is two slots at a 16K window, but a call the engine sizes at 32K loads the 27B at 2 x 32K, which the budget puts under the runtime's margin (1,429 MiB left, 1,936 MiB wanted; spec section 5). That is derived from the runtime's own target and has not been observed on this card, so observe it once**

A load at 2 x 32K is a runner started with `-c 65536 -np 2` (`-c` is the window of every slot together; the census prints it per slot as `32768 x2`). If a real call has needed 32K since Step 5, the log already holds that load and the first command below finds it; if none has, make one. **THIS RELOADS THE 27B AT 32K (a few seconds) with the embedder resident, and the next call at 16K reloads it back, so the census shows two extra window-change loads that day.**

```bash
curl -s http://127.0.0.1:11434/api/generate -d '{"model":"qwen3.8:27b","keep_alive":"2h","options":{"num_ctx":32768}}'
echo
LINE=$(grep -n 'starting llama-server.* -c 65536 -np 2' ~/.memql/state/ollama.log | tail -1 | cut -d: -f1)
tail -n +"$LINE" ~/.memql/state/ollama.log | grep -E 'projected to use|will leave|offloaded' | head -3 | cut -c1-170
curl -s http://127.0.0.1:11434/api/ps | python3 -I -c 'import sys,json; [print(m["model"], m["size_vram"]*100//m["size"], "% on the GPU, context", m["context_length"]) for m in json.load(sys.stdin)["models"]]'
```

Expected if the budget holds: the margin line with `>=` and `no changes needed`, `offloaded N/N layers to GPU` with N equal to the model's layer count (66 for this model, as every load in the thirty days showed), and both models loaded, each 100% on the GPU:

```text
{"model":"qwen3.8:27b","created_at":"...","response":"","done":true,"done_reason":"load"}
... common_params_fit_impl: projected to use <N> MiB of device memory vs. <M> MiB of free device memory
... common_params_fit_impl: will leave <K> >= 1936 MiB of free device memory, no changes needed
... load_tensors: offloaded 66/66 layers to GPU
qwen3.8:27b 100 % on the GPU, context 32768
qwen3-embedding:0.6b 100 % on the GPU, context 8192
```

If any of it does not hold (`will leave` below 1936, fewer layers on the GPU than the model has, a model under 100%, or the embedder gone from `/api/ps`), give Ollama one slot, the way Step 2 gave it two: set `Environment=OLLAMA_NUM_PARALLEL=1` in the drop-in, `systemctl --user daemon-reload`, then `systemctl --user restart memql-ollama.service` (DISRUPTIVE, as Step 3), then warm both models again (Step 5). The worker follows the unit within a refresh, and `memql worker models` then reads `Requests per Ollama model: 1, from the memql-ollama.service unit.` With one slot the 32K window leaves 3,893 MiB.

- [ ] **Step 9: ROLLBACK, in order of how much each disturbs**

```bash
# 1. Residency off. Instant, nothing restarts: delete the resident: block from ~/.memql/policy.yaml, then
kill -HUP "$(systemctl --user show memql-worker.service -p MainPID --value)"
# Optional: unload now instead of waiting out the two hours.
~/.memql/ollama/runtime/bin/ollama stop qwen3.8:27b
~/.memql/ollama/runtime/bin/ollama stop qwen3-embedding:0.6b

# 2. One slot again. DISRUPTIVE (restarts Ollama).
rm ~/.config/systemd/user/memql-ollama.service.d/10-fleet-tuning.conf
rmdir ~/.config/systemd/user/memql-ollama.service.d
systemctl --user daemon-reload
systemctl --user restart memql-ollama.service

# 3. The old binary: Task 12, ROLLBACK.

# 4. When the work is finished: remove the pinned worktree and the scratch.
git -C /home/znas/memql-projects/memql worktree remove --force /home/znas/memql-projects/memql-pin-fleet
```

Nothing is printed except, from `ollama stop`, an error for a model that was not loaded, which is harmless. Afterwards `memql worker models` reads `Requests per Ollama model: 1, from the default.` and `nothing is configured to stay loaded`.
