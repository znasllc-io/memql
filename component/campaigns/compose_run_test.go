package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/compose"
)

// compose_run_test.go -- one email composition run (campaignRunComposition,
// the step of the composeEmailRun template): who it hands the composition to,
// with which tools and which files, and how the run and the composition end.
//
// The composer is the engine's bounded tool loop, which on the Apps route hands
// the step whole to Claude Code. Here the engine records what it was asked and
// stands in for the session: it may call the real tools with the arguments the
// run stamped, as the session's MCP calls would.

// runGoal is the goal of the run in these tests, written as the run context
// carries it; the composition records the same goal bare (composeGoal).
const runGoal = "v1:work:goal:" + composeGoal

// composerCall is one call of the bounded tool loop, as the run made it.
type composerCall struct {
	prompt   string
	data     map[string]any
	tools    []string
	agentID  string
	role     string
	defaults map[string]map[string]any
	inputs   []string
	run      common.RunContext
}

// composerEngine is the compose engine with the bounded tool loop a run calls.
// session, when set, is what the composer does with the call; nil answers at
// once and saves nothing.
type composerEngine struct {
	*composeEngine
	aiMu    sync.Mutex
	asked   []composerCall
	session func(ctx context.Context) (string, error)
}

func (e *composerEngine) InvokeAIChatWithFilteredTools(ctx context.Context, templateID string, data map[string]any, toolNames []string) (string, error) {
	run, _ := common.RunFromContext(ctx)
	e.aiMu.Lock()
	e.asked = append(e.asked, composerCall{
		prompt: templateID, data: maps.Clone(data), tools: slices.Clone(toolNames),
		agentID: memql.ActingAgentIdFromContext(ctx), role: memql.ActingAgentRoleFromContext(ctx),
		defaults: common.DelegatedToolDefaultsFromContext(ctx), inputs: memql.SessionInputsFromContext(ctx), run: run,
	})
	session := e.session
	e.aiMu.Unlock()
	if session == nil {
		return "I read the material.", nil
	}
	return session(ctx)
}

func (e *composerEngine) calls() []composerCall {
	e.aiMu.Lock()
	defer e.aiMu.Unlock()
	return slices.Clone(e.asked)
}

// filedFile is one file the run asked to have filed.
type filedFile struct {
	owner, folder, name, mime string
	data                      []byte
}

// recordingFiler is a MaterialFiler that keeps what it is asked. failAt, when
// set, is the File call (counted from 1) that fails after its file is written.
type recordingFiler struct {
	mu       sync.Mutex
	filed    []filedFile
	archived []FiledMaterial
	owners   []string
	failAt   int
}

func (f *recordingFiler) File(_ context.Context, owner, folder, name, mime string, data []byte) (FiledMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filed = append(f.filed, filedFile{owner, folder, name, mime, slices.Clone(data)})
	n := len(f.filed)
	got := FiledMaterial{FileID: fmt.Sprintf("material-file-%d", n), ArtifactID: fmt.Sprintf("material-artifact-%d", n)}
	if n == f.failAt {
		return FiledMaterial{FileID: got.FileID}, errors.New("its Library entry did not appear in time")
	}
	return got, nil
}

func (f *recordingFiler) Archive(_ context.Context, owner string, m FiledMaterial) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived = append(f.archived, m)
	f.owners = append(f.owners, owner)
	return nil
}

func (f *recordingFiler) snapshot() (filed []filedFile, archived []FiledMaterial, owners []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.filed), slices.Clone(f.archived), slices.Clone(f.owners)
}

// runWorld is a compose world whose engine has the tool loop and whose worker
// files material with a recordingFiler.
type runWorld struct {
	*composeWorld
	ai    *composerEngine
	filer *recordingFiler
}

func newRunWorld(t *testing.T) *runWorld {
	t.Helper()
	cw := newComposeWorld(t)
	ai := &composerEngine{composeEngine: cw.engine}
	cw.w.store = NewStore(ai)
	filer := &recordingFiler{}
	cw.w.SetMaterialFiler(filer)
	return &runWorld{composeWorld: cw, ai: ai, filer: filer}
}

// runContext is the context the work spine runs the template's step under: the
// owner's own authority and the run, its goal, its step and its route.
func runContext(goalID string) context.Context {
	return common.ContextWithRun(importCtx(), common.RunContext{
		RunId: "v1:work:run:run-1", GoalId: goalID, OwnerUserId: "v1:identity:user:" + testOwner,
		StepKey: "composeEmail", Mode: common.RunModeLive,
		Routing: common.RouteChoice{Source: "route:apps", Level: "strong", By: testOwner},
	})
}

// run runs the step for the template under ctx.
func (rw *runWorld) run(ctx context.Context, templateID string) (map[string]any, error) {
	rw.t.Helper()
	nodes, err := rw.w.handleRunComposition(ctx, map[string]any{"templateId": templateID}, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 || nodes[0].ID != "campaignCompositionRun" {
		rw.t.Fatalf("expected one campaignCompositionRun node, got %+v", nodes)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		rw.t.Fatal(err)
	}
	return out, nil
}

// submitting is a session that saves the composition with the arguments the
// run stamped for emailComposeSubmit, as the session's MCP call would.
func (rw *runWorld) submitting(c map[string]any) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		args := map[string]any{"composition": c}
		maps.Copy(args, common.DelegatedToolDefaultsFromContext(ctx)["emailComposeSubmit"])
		if _, err := rw.w.handleComposeSubmit(ctx, args, 0); err != nil {
			return "", err
		}
		return "Saved the draft.", nil
	}
}

func TestRunCompositionCallsTheComposerWithExactlyTheEmailTools(t *testing.T) {
	rw := newRunWorld(t)
	rw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := rw.addFile("hero.png", "image/png", hero)
	copyID := rw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
	zipID := rw.addFile("STAGE B - 1.zip", "application/zip", zipOf(t, zipEntry{"copy.txt", "The garden is waking up."}))
	finished := previewPNG(t, 8, 8)
	referenceID := rw.addFile("finished.png", "image/png", finished)
	id := rw.start("builtin:article", []string{heroID, copyID, zipID}, referenceID)
	rw.ai.session = rw.submitting(articleComposition(compose.EmailAssetHandle(hero)))

	out, err := rw.run(runContext(runGoal), id)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	calls := rw.ai.calls()
	if len(calls) != 1 {
		t.Fatalf("the composer was called %d times, want once", len(calls))
	}
	call := calls[0]
	if call.prompt != "emailCompose" {
		t.Errorf("prompt = %q, want emailCompose", call.prompt)
	}
	tools := []string{"emailComposeContext", "emailComposeRender", "emailComposeSubmit"}
	if !slices.Equal(call.tools, tools) {
		t.Errorf("tools = %v, want exactly %v", call.tools, tools)
	}
	if call.agentID != "emailComposerAgent-"+testOwner || call.role != "specialist" {
		t.Errorf("acting agent = %q as %q, want emailComposerAgent-%s as specialist", call.agentID, call.role, testOwner)
	}
	if len(call.defaults) != len(tools) {
		t.Errorf("stamped tools = %v, want the three", slices.Sorted(maps.Keys(call.defaults)))
	}
	for _, tool := range tools {
		stamped := call.defaults[tool]
		if len(stamped) != 2 || stamped["templateId"] != id || !sameGoal(fmt.Sprint(stamped["goalId"]), runGoal) {
			t.Errorf("%s is stamped %v, want templateId %s and the run's goal", tool, stamped, id)
		}
	}
	if call.run.StepKey != "composeEmail" || call.run.GoalId != runGoal {
		t.Errorf("the composer was called outside the run's step: %+v", call.run)
	}

	// Every file of the material is filed under its workspace name, the
	// picture of the finished email included, and handed to the session.
	filed, _, _ := rw.filer.snapshot()
	var names, artifacts []string
	for i, f := range filed {
		names = append(names, f.name)
		artifacts = append(artifacts, fmt.Sprintf("material-artifact-%d", i+1))
		if f.owner != "v1:identity:user:"+testOwner || f.folder != "Email material" {
			t.Errorf("%s was filed for %q in %q", f.name, f.owner, f.folder)
		}
	}
	if want := []string{"hero.png", "copy.txt", "STAGE B - 1__copy.txt", "finished.png"}; !slices.Equal(names, want) {
		t.Errorf("filed %v, want %v", names, want)
	}
	if !slices.Equal(call.inputs, artifacts) {
		t.Errorf("session inputs = %v, want the filed artifacts %v", call.inputs, artifacts)
	}
	if string(filed[0].data) != string(hero) || filed[0].mime != "image/png" || filed[1].mime != "text/plain" {
		t.Errorf("the hero was filed as %s (%d bytes), the copy as %s", filed[0].mime, len(filed[0].data), filed[1].mime)
	}

	// The prompt's data: the description, and the context as JSON.
	if call.data["description"] != "Tell them about spring." {
		t.Errorf("description = %v", call.data["description"])
	}
	text, _ := call.data["context"].(string)
	var context map[string]any
	if err := json.Unmarshal([]byte(text), &context); err != nil {
		t.Fatalf("the context is not JSON: %v\n%.200s", err, text)
	}
	if context["templateId"] != id || context["reference"] != "finished.png" || len(listOf(context["files"])) != 4 {
		t.Errorf("context: template %v reference %v files %v", context["templateId"], context["reference"], context["files"])
	}

	// The session submitted: the composition is done, and the run says so.
	done, _ := compositionFromRow(rw.engine.row(id))
	if done.State != compositionDone || out["state"] != compositionDone || out["templateId"] != id {
		t.Errorf("composition %s, answer %v", done.State, out)
	}
}

// startWithoutGoal makes a template whose composition is writing and records
// no goal yet: the run can start before campaignComposeEmail's write lands.
func (rw *runWorld) startWithoutGoal(layoutID string, fileIDs []string) string {
	rw.t.Helper()
	c := composition{State: compositionWriting, StartedAt: templateNow,
		Request: compositionRequest{LayoutID: layoutID, Name: "Spring news", Description: "Tell them about spring.", FileIDs: fileIDs}}
	id, _, _, err := rw.w.createTemplateFromLayout(importCtx(), templateOrg, "Spring news", layoutID, &c)
	if err != nil {
		rw.t.Fatalf("createTemplateFromLayout: %v", err)
	}
	return id
}

// The sentences a failed composition records, which a person reads on the
// template, on the OS list and as the editor's bar: fixed and short, never a
// cause's own words.
const (
	saidNothingSaved   = "MemQL finished without saving the email."
	saidAppUnreachable = "Your Claude Code app could not be reached."
	saidCouldNotFinish = "MemQL could not finish this email."
	saidComposeBusy    = "Composing is busy right now. Try again in a moment."
	saidNoUsableFiles  = "None of the attached files can be used."
	saidUnreadableFile = "A file could not be read from your Library."
)

// logsTo sends the worker's warnings to a buffer the test reads.
func (rw *runWorld) logsTo() *syncBuffer {
	buf := &syncBuffer{}
	rw.w.logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return buf
}

func TestRunCompositionMarksFailedWhenNothingIsSubmitted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session func(context.Context) (string, error)
		reason  string
		cause   string // the cause's own words, logged and never recorded
	}{
		{"the composer saved nothing", nil, saidNothingSaved, ""},
		// Cockpit is closed, or the machine is asleep: no app door is open.
		{"no app door is open", func(context.Context) (string, error) {
			return "", &memql.AppUnavailable{AppId: "*", Considered: map[string]string{"claude-code": "the machine went to sleep"}}
		}, saidAppUnreachable, "no app door is open"},
		{"the route cannot serve the person now", func(context.Context) (string, error) {
			return "", fmt.Errorf("route: %w", &memql.RouteChoiceError{Code: memql.RouteNotReady, Field: "source", Value: "route:apps", Reason: "no signed-in app"})
		}, saidAppUnreachable, "no signed-in app"},
		// A refusal that crossed a wire keeps only its words, which lead with its code.
		{"a refusal as words", func(context.Context) (string, error) {
			return "", errors.New(memql.AppRefusalCode + ": no machine can run claude-code; last attempt: dial tcp 10.0.0.7:443: i/o timeout")
		}, saidAppUnreachable, "dial tcp 10.0.0.7:443"},
		{"the composer failed otherwise", func(context.Context) (string, error) {
			return "", errors.New(`app door "claude-code": pq: connection reset by peer`)
		}, saidCouldNotFinish, "pq: connection reset by peer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rw := newRunWorld(t)
			rw.withKit()
			logged := rw.logsTo()
			copyID := rw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
			readmeID := rw.addFile("readme.pdf", "application/pdf", []byte("%PDF-1.4"))
			id := rw.start("builtin:article", []string{copyID, readmeID}, "")
			before := rw.engine.row(id)
			rw.ai.session = tc.session

			_, err := rw.run(runContext(runGoal), id)
			if err == nil {
				t.Fatal("a run whose composition was not saved succeeded")
			}
			// The composition recorded its own failure: the work spine fails
			// the run rather than retrying a step that would find it failed.
			if code, terminal := workstate.TerminalFailureCode(err.Error()); !terminal || code != workstate.TerminalCompositionFailed {
				t.Errorf("run error %q is not the terminal composition failure", err)
			}
			row := rw.engine.row(id)
			c, _ := compositionFromRow(row)
			if c.State != compositionFailed || c.Error != tc.reason || !c.FinishedAt.Equal(templateNow) {
				t.Errorf("composition = %s %q at %v, want failed %q", c.State, c.Error, c.FinishedAt, tc.reason)
			}
			if !sameGoal(c.GoalID, runGoal) || !slices.Equal(c.Notes, []string{"Left out readme.pdf: not a picture or text file."}) {
				t.Errorf("goal %q, notes %q", c.GoalID, c.Notes)
			}
			if !sameJSON(t, row["document"], before["document"]) || row["status"] != before["status"] {
				t.Error("failing the composition changed the email")
			}
			// The cause is logged with the template, and is nowhere a person
			// reads it: not on the template, and not in the run's error.
			if tc.cause != "" {
				stored, _ := plainJSON(row)
				if strings.Contains(string(stored), tc.cause) || strings.Contains(err.Error(), tc.cause) {
					t.Errorf("the cause reached the person: %s", err)
				}
				if text := logged.String(); !strings.Contains(text, tc.cause) || !strings.Contains(text, id) {
					t.Errorf("the cause was not logged with the template: %s", text)
				}
			}
		})
	}

	t.Run("composing was busy", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.start("builtin:article", []string{rw.addFile("copy.txt", "text/plain", []byte("Hi."))}, "")
		setDuration(t, &renderSlotWait, 50*time.Millisecond)
		holdRenderSlots(t, maxConcurrentRenders)
		if _, err := rw.run(runContext(runGoal), id); err == nil {
			t.Fatal("the run succeeded without a render slot")
		}
		if c, _ := compositionFromRow(rw.engine.row(id)); c.State != compositionFailed || c.Error != saidComposeBusy {
			t.Errorf("composition = %s %q", c.State, c.Error)
		}
	})

	t.Run("none of the files can be used", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.start("builtin:article", []string{
			rw.addFile("readme.pdf", "application/pdf", []byte("%PDF-1.4")), rw.addFile("logo.svg", "image/svg+xml", []byte("<svg/>")),
		}, "")
		if _, err := rw.run(runContext(runGoal), id); err == nil {
			t.Fatal("the run succeeded with nothing to write from")
		}
		// The sentence is short; which files, and why, are its notes.
		c, _ := compositionFromRow(rw.engine.row(id))
		want := []string{"Left out readme.pdf: not a picture or text file.", "Left out logo.svg: svg is not a picture an email can carry."}
		if c.State != compositionFailed || c.Error != saidNoUsableFiles || !slices.Equal(c.Notes, want) {
			t.Errorf("composition = %s %q, notes %q", c.State, c.Error, c.Notes)
		}
	})

	t.Run("a file could not be read", func(t *testing.T) {
		rw := newRunWorld(t)
		logged := rw.logsTo()
		copyID := rw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
		delete(rw.blobs.objects, "library/"+testOwner+"/"+copyID+"/blob") // storage has lost it
		id := rw.start("builtin:article", []string{copyID}, "")
		if _, err := rw.run(runContext(runGoal), id); err == nil {
			t.Fatal("the run succeeded without its file")
		}
		c, _ := compositionFromRow(rw.engine.row(id))
		want := []string{"copy.txt could not be read from your Library. Try again in a moment, or attach it again."}
		if c.State != compositionFailed || c.Error != saidUnreadableFile || !slices.Equal(c.Notes, want) {
			t.Errorf("composition = %s %q, notes %q", c.State, c.Error, c.Notes)
		}
		if !strings.Contains(logged.String(), copyID) {
			t.Errorf("the read failure was not logged: %s", logged.String())
		}
	})

	t.Run("the run was cancelled", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.start("builtin:article", nil, "")
		rw.ai.session = func(context.Context) (string, error) { return "", context.Canceled }
		_, err := rw.run(runContext(runGoal), id)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v, want the cancellation", err)
		}
		if _, terminal := workstate.TerminalFailureCode(err.Error()); terminal {
			t.Errorf("a cancellation read as a terminal failure: %v", err)
		}
		if c, _ := compositionFromRow(rw.engine.row(id)); c.State != compositionFailed || c.Error != "Cancelled" {
			t.Errorf("composition = %s %q, want failed Cancelled", c.State, c.Error)
		}
	})

	t.Run("a composition already settled stays as it is", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.start("builtin:article", nil, "")
		writes := 0
		rw.ai.session = func(context.Context) (string, error) {
			rw.setState(id, compositionFailed) // cancelled while the composer ran
			writes = len(rw.engine.writes())
			return "", errors.New("the session was stopped")
		}
		out, err := rw.run(runContext(runGoal), id)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(rw.engine.writes()) != writes || out["state"] != compositionFailed {
			t.Errorf("the run wrote over a settled composition: %d writes after, answer %v", len(rw.engine.writes())-writes, out)
		}
	})
}

func TestRunCompositionArchivesItsMaterialFiles(t *testing.T) {
	setup := func(t *testing.T) (*runWorld, string, []string) {
		rw := newRunWorld(t)
		rw.withKit()
		hero := previewPNG(t, 6, 4)
		heroID := rw.addFile("hero.png", "image/png", hero)
		copyID := rw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
		zipID := rw.addFile("STAGE B - 1.zip", "application/zip", zipOf(t,
			zipEntry{"a/email.txt", "One."}, zipEntry{"b/email.txt", "Two."}))
		id := rw.start("builtin:article", []string{heroID, copyID, zipID}, "")
		return rw, id, []string{heroID, copyID, zipID}
	}
	wantFiled := []FiledMaterial{
		{"material-file-1", "material-artifact-1"}, {"material-file-2", "material-artifact-2"},
		{"material-file-3", "material-artifact-3"}, {"material-file-4", "material-artifact-4"},
	}
	for _, tc := range []struct {
		name   string
		submit bool
	}{{"after a run that saved the email", true}, {"after a run that saved nothing", false}} {
		t.Run(tc.name, func(t *testing.T) {
			rw, id, attachments := setup(t)
			archivedDuringTheSession := -1
			save := rw.submitting(articleComposition(""))
			rw.ai.session = func(ctx context.Context) (string, error) {
				_, archived, _ := rw.filer.snapshot()
				archivedDuringTheSession = len(archived)
				if tc.submit {
					return save(ctx)
				}
				return "", nil
			}
			_, err := rw.run(runContext(runGoal), id)
			if tc.submit && err != nil {
				t.Fatalf("run: %v", err)
			}
			filed, archived, owners := rw.filer.snapshot()
			if len(filed) != 4 || archivedDuringTheSession != 0 {
				t.Fatalf("filed %d files; %d were archived while the composer worked", len(filed), archivedDuringTheSession)
			}
			if !slices.Equal(archived, wantFiled) {
				t.Errorf("archived %v, want every filed file once: %v", archived, wantFiled)
			}
			for _, owner := range owners {
				if owner != "v1:identity:user:"+testOwner {
					t.Errorf("archived as %q", owner)
				}
			}
			// The person's own attachments are never archived.
			for _, w := range rw.engine.writes() {
				for _, attachment := range attachments {
					if name, args := parseRenderedCall(w.query); args["fileId"] == attachment || args["artifactId"] == attachment {
						t.Errorf("%s reached the person's attachment %s", name, attachment)
					}
				}
			}
		})
	}

	t.Run("when filing fails partway", func(t *testing.T) {
		rw, id, _ := setup(t)
		logged := rw.logsTo()
		rw.filer.failAt = 2
		_, err := rw.run(runContext(runGoal), id)
		if code, terminal := workstate.TerminalFailureCode(fmt.Sprint(err)); !terminal || code != workstate.TerminalCompositionFailed {
			t.Fatalf("run error = %v", err)
		}
		if calls := rw.ai.calls(); len(calls) != 0 {
			t.Fatal("the composer was called without its material")
		}
		_, archived, _ := rw.filer.snapshot()
		if want := []FiledMaterial{{"material-file-1", "material-artifact-1"}, {FileID: "material-file-2"}}; !slices.Equal(archived, want) {
			t.Errorf("archived %v, want the files filed so far %v", archived, want)
		}
		c, _ := compositionFromRow(rw.engine.row(id))
		if c.State != compositionFailed || c.Error != saidCouldNotFinish {
			t.Errorf("composition = %s %q, want failed %q", c.State, c.Error, saidCouldNotFinish)
		}
		// Which file, and why, is the log's to say.
		if text := logged.String(); !strings.Contains(text, "copy.txt") || !strings.Contains(text, "its Library entry did not appear in time") {
			t.Errorf("the cause was not logged: %s", text)
		}
	})
}

func TestRunCompositionClaimsItsCompositionForItsGoal(t *testing.T) {
	t.Run("a composition with no goal yet becomes this run's", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.startWithoutGoal("builtin:article", nil)
		rw.ai.session = rw.submitting(articleComposition(""))
		if _, err := rw.run(runContext(runGoal), id); err != nil {
			t.Fatalf("run: %v", err)
		}
		call := rw.ai.calls()[0]
		if got := call.defaults["emailComposeSubmit"]["goalId"]; got != runGoal {
			t.Errorf("the tools are stamped with goal %v, want the run's", got)
		}
		row := rw.engine.row(id)
		c, _ := compositionFromRow(row)
		// The composition started from the template as it was, so the session's
		// submit saved over it rather than beside it.
		if !sameGoal(c.GoalID, runGoal) || c.State != compositionDone || len(rw.engine.rows) != 1 {
			t.Errorf("composition goal %q state %s; %d templates", c.GoalID, c.State, len(rw.engine.rows))
		}
	})

	t.Run("a composition another goal writes is left alone", func(t *testing.T) {
		rw := newRunWorld(t)
		id := rw.start("builtin:article", nil, "") // goal composeGoal
		before := rw.engine.row(id)
		writes := len(rw.engine.writes())
		out, err := rw.run(runContext("v1:work:goal:goal-old"), id)
		if err != nil {
			t.Fatalf("a stale run failed: %v", err)
		}
		filed, archived, _ := rw.filer.snapshot()
		if len(rw.ai.calls()) != 0 || len(filed) != 0 || len(archived) != 0 || len(rw.engine.writes()) != writes {
			t.Errorf("a stale run acted: %d composer calls, %d filed, %d writes", len(rw.ai.calls()), len(filed), len(rw.engine.writes())-writes)
		}
		if !sameJSON(t, rw.engine.row(id), before) || out["skipped"] == nil || out["state"] != compositionWriting {
			t.Errorf("answer %v", out)
		}
	})
}

func TestRunCompositionDoesNothingUnlessWriting(t *testing.T) {
	for _, state := range []string{compositionDone, compositionFailed} {
		t.Run(state, func(t *testing.T) {
			rw := newRunWorld(t)
			id := rw.start("builtin:article", []string{rw.addFile("copy.txt", "text/plain", []byte("Hi."))}, "")
			rw.setState(id, state)
			writes := len(rw.engine.writes())
			out, err := rw.run(runContext(runGoal), id)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			filed, _, _ := rw.filer.snapshot()
			if len(rw.ai.calls()) != 0 || len(filed) != 0 || len(rw.engine.writes()) != writes || out["state"] != state {
				t.Errorf("a %s composition was acted on: answer %v", state, out)
			}
		})
	}
}

func TestRunCompositionRunsOnlyAsItsGoalOnTheAppsRoute(t *testing.T) {
	with := func(change func(*common.RunContext)) context.Context {
		run, _ := common.RunFromContext(runContext(runGoal))
		change(&run)
		return common.ContextWithRun(importCtx(), run)
	}
	for name, ctx := range map[string]context.Context{
		"no run":                importCtx(),
		"a run without a goal":  with(func(r *common.RunContext) { r.GoalId = "" }),
		"somebody else's run":   with(func(r *common.RunContext) { r.OwnerUserId = "v1:identity:user:someone-else" }),
		"the rules decide":      with(func(r *common.RunContext) { r.Routing.Source = "" }),
		"route:auto":            with(func(r *common.RunContext) { r.Routing.Source = "route:auto" }),
		"route:local":           with(func(r *common.RunContext) { r.Routing.Source = "route:local" }),
		"a fleet model":         with(func(r *common.RunContext) { r.Routing.Source = "fleet:strongest" }),
		"a step override model": with(func(r *common.RunContext) { r.Override = &common.StepOverride{Model: "fleet:qwen3"} }),
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t)
			id := rw.start("builtin:article", []string{rw.addFile("copy.txt", "text/plain", []byte("Hi."))}, "")
			before := rw.engine.row(id)
			writes := len(rw.engine.writes())
			if _, err := rw.run(ctx, id); err == nil {
				t.Fatal("the run was not refused")
			}
			filed, _, _ := rw.filer.snapshot()
			if len(rw.ai.calls()) != 0 || len(filed) != 0 || len(rw.engine.writes()) != writes || !sameJSON(t, rw.engine.row(id), before) {
				t.Error("a refused run touched the composition, its files or the composer")
			}
		})
	}
	if err := appsOnly(common.RunContext{Routing: common.RouteChoice{Source: "route:apps"}, Override: &common.StepOverride{Model: "app:claude-code:claude-opus-5"}}); err != nil {
		t.Errorf("a person's override to an app model was refused: %v", err)
	}
}

func TestRunCompositionNeedsItsLibraryWriter(t *testing.T) {
	rw := newRunWorld(t)
	rw.w.SetMaterialFiler(nil)
	id := rw.start("builtin:article", []string{rw.addFile("copy.txt", "text/plain", []byte("Hi."))}, "")
	if _, err := rw.run(runContext(runGoal), id); err == nil {
		t.Fatal("a run with files and no Library writer succeeded")
	}
	if len(rw.ai.calls()) != 0 {
		t.Error("the composer was called without its files")
	}
	if c, _ := compositionFromRow(rw.engine.row(id)); c.State != compositionFailed || c.Error != saidCouldNotFinish {
		t.Errorf("composition = %s %q", c.State, c.Error)
	}

	// A composition with no files needs none.
	rw = newRunWorld(t)
	rw.w.SetMaterialFiler(nil)
	id = rw.start("builtin:article", nil, "")
	rw.ai.session = rw.submitting(articleComposition(""))
	if _, err := rw.run(runContext(runGoal), id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls := rw.ai.calls(); len(calls) != 1 || len(calls[0].inputs) != 0 {
		t.Errorf("composer calls %d", len(calls))
	}
}

// TestCompositionReasonIsAFixedSentence: what a failed composition records is
// one of a few fixed sentences, each at most ten words, never the cause's own
// words; the package's own sentences pass through, and the files left out are
// the notes, not the sentence.
func TestCompositionReasonIsAFixedSentence(t *testing.T) {
	unusable := &unusableMaterialError{notes: []string{"Left out a.pdf: not a picture or text file."}}
	for _, tc := range []struct {
		err   error
		want  string
		notes []string
	}{
		{nil, saidNothingSaved, nil},
		{fmt.Errorf("wrapped: %w", context.Canceled), "Cancelled", nil},
		{fmt.Errorf("app session: %w", &memql.AppUnavailable{AppId: "*"}), saidAppUnreachable, nil},
		{fmt.Errorf("%w: no machine is signed in", memql.ErrAppUnavailable), saidAppUnreachable, nil},
		{&memql.RouteChoiceError{Code: memql.RouteNotReady, Reason: "no signed-in app"}, saidAppUnreachable, nil},
		{errors.New("route_not_ready: route:apps cannot serve this person right now"), saidAppUnreachable, nil},
		{errComposeBusy, saidComposeBusy, nil},
		{errNothingSaved, saidNothingSaved, nil},
		{fmt.Errorf("material: %w", unusable), saidNoUsableFiles, unusable.notes},
		{&unreadableMaterialError{name: "copy.txt"}, saidUnreadableFile, []string{"copy.txt could not be read from your Library. Try again in a moment, or attach it again."}},
		{errors.New("the machine went to sleep"), saidCouldNotFinish, nil},
		{errors.New("pq: connection reset by peer on goal insert"), saidCouldNotFinish, nil},
		{context.DeadlineExceeded, saidCouldNotFinish, nil},
		{errNoComposerLoop, saidCouldNotFinish, nil},
	} {
		got, notes := compositionReason(tc.err)
		if got != tc.want || !slices.Equal(notes, tc.notes) {
			t.Errorf("compositionReason(%v) = %q %q, want %q %q", tc.err, got, notes, tc.want, tc.notes)
		}
		if n := len(strings.Fields(got)); n > 10 {
			t.Errorf("%q is %d words", got, n)
		}
	}
}

// TestComposeRunLoadsWithItsPromptAndTemplate boots the shipped DSL with this
// integration registered: the builtin runs the integration's capability, the
// prompt declares the strong level and renders the run's data with its rules,
// and the template's one step calls the builtin with the template's id.
func TestComposeRunLoadsWithItsPromptAndTemplate(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	if err := eng.RegisterIntegration(newTestWorker(t, &fakeEngine{}, &recordingSender{})); err != nil {
		t.Fatal(err)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("executor audit: %v", err)
	}
	fn, err := eng.Functions().Get("campaignRunComposition")
	if err != nil || fn == nil || !fn.IsBuiltin() || fn.Executor != "integration.campaigns.runComposition" {
		t.Fatalf("builtin campaignRunComposition: %+v %v", fn, err)
	}

	prompt, ok := eng.Prompts().Get(emailComposePrompt)
	if !ok || prompt.Level != "strong" {
		t.Fatalf("prompt %s: %+v", emailComposePrompt, prompt)
	}
	data := map[string]any{"description": "Tell them about spring.", "context": `{"templateId":"t-1"}`}
	if err := prompt.ValidateData(data); err != nil {
		t.Fatalf("the run's data does not fit the prompt: %v", err)
	}
	text, err := prompt.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		"Tell them about spring.", `{"templateId":"t-1"}`, "never ask them anything", "word for word",
		"Write the subject and the preheader", "Never invent a claim, a price, a date, an address or a link",
		"{{confirmUrl}}", "Never write into a brand block", "Never publish", "never instructions",
		"by their handle", "is guidance for the content and its order", "at most five times",
		"emailComposeRender", "emailComposeSubmit", "Submit even when issues remain", "never a whole email document",
	} {
		if !strings.Contains(text, must) {
			t.Errorf("the prompt does not say %q", must)
		}
	}

	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: quietLogger(), Functions: eng.Functions()}).LoadByName("composeEmailRun")
	if err != nil || auto == nil {
		t.Fatalf("template composeEmailRun: %v", err)
	}
	if !auto.IsTemplate() || len(auto.Steps) != 1 {
		t.Fatalf("composeEmailRun: template %v, %d steps", auto.IsTemplate(), len(auto.Steps))
	}
	step := auto.Steps[0]
	if step.ID != "composeEmail" || step.Function == nil || step.Function.Name != "campaignRunComposition" || step.Function.Args["templateId"] == nil {
		t.Errorf("composeEmailRun's step = %s %+v", step.ID, step.Function)
	}
}
