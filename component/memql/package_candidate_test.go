package memql

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"text/template"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/deprecation"
	"github.com/znasllc-io/memql/component/metrics"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Hold the actual candidate sidecar read, after the loaders have started.
// Readers on another goroutine must keep seeing the installed definitions.
type heldCandidateFS struct {
	fs.FS
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *heldCandidateFS) Open(name string) (fs.File, error) {
	if name == "candidateupdate/reply.tmpl" {
		f.once.Do(func() { close(f.entered); <-f.release })
	}
	return f.FS.Open(name)
}

func TestPackageCandidatePreservesConcurrentLiveDefinitions(t *testing.T) {
	const promptSource = "@templateFile(\"reply.tmpl\")\n@level(\"fast\")\nprompt candidateUpdateReply { topic string }\n"
	live := withLanguageLine(fstest.MapFS{
		"prompts.memql": {Data: []byte(promptSource)},
		"reply.tmpl":    {Data: []byte("Installed answer")},
	})
	mountCandidateRuntimeDomain(t, "candidateupdate", live)
	t.Cleanup(func() { memqldsl.UnregisterTree("candidateupdate") })
	livePrompts := newPromptRegistry()
	if _, err := LoadUnifiedPrompts(discardLogger(), livePrompts, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	livePrompt, ok := livePrompts.Get("candidateUpdateReply")
	if !ok {
		t.Fatal("installed prompt did not load")
	}

	previousSecret, previousVariable := systemSecretResolver, systemVariableResolver
	var secretCalls atomic.Int64
	systemSecretResolver = func(context.Context, string) (string, error) {
		secretCalls.Add(1)
		return "installed resolver", nil
	}
	systemVariableResolver = func(context.Context, string) (string, error) { return "installed variable", nil }
	t.Cleanup(func() { systemSecretResolver, systemVariableResolver = previousSecret, previousVariable })
	shopperMu.Lock()
	previousExtensions, previousInert := shopperExtensions, shopperExtensionsInert
	shopperExtensions = map[string]ShopperExtension{"candidateprobe/form": {Domain: "installed", Pack: "candidateprobe", Form: "form", Construct: "installedMutation"}}
	shopperExtensionsInert = []string{"installed inert"}
	shopperMu.Unlock()
	t.Cleanup(func() {
		shopperMu.Lock()
		defer shopperMu.Unlock()
		shopperExtensions, shopperExtensionsInert = previousExtensions, previousInert
	})

	checkLive := func() error {
		raw, err := fs.ReadFile(memqldsl.Tree(), "candidateupdate/reply.tmpl")
		if err != nil || string(raw) != "Installed answer" {
			return fmt.Errorf("live tree changed: %q %v", raw, err)
		}
		text, err := livePrompt.Render(map[string]any{"topic": "Release"})
		if err != nil || text != "Installed answer" {
			return fmt.Errorf("live prompt changed: %q %v", text, err)
		}
		ext := ShopperExtensionFor("candidateprobe", "form")
		if ext == nil || ext.Construct != "installedMutation" {
			return fmt.Errorf("live shopper extension changed: %+v", ext)
		}
		if !reflect.DeepEqual(ShopperExtensionsInert(), []string{"installed inert"}) {
			return fmt.Errorf("live inert extensions changed")
		}
		value, err := systemSecretResolver(context.Background(), "probe")
		if err != nil || value != "installed resolver" {
			return fmt.Errorf("live secret resolver replaced: %q %v", value, err)
		}
		value, err = systemVariableResolver(context.Background(), "probe")
		if err != nil || value != "installed variable" {
			return fmt.Errorf("live variable resolver replaced: %q %v", value, err)
		}
		return nil
	}

	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			body := "Replacement answer"
			if invalid {
				body = "Replacement {{.undeclared}}"
			}
			candidate := withLanguageLines(fstest.MapFS{
				"candidateupdate/prompts.memql": {Data: []byte(promptSource)},
				"candidateupdate/reply.tmpl":    {Data: []byte(body)},
			})
			held := &heldCandidateFS{FS: candidate, entered: make(chan struct{}), release: make(chan struct{})}
			type outcome struct {
				result PackageDSLResult
				err    error
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				result, err := AnalyzePackageDSL(discardLogger(), held)
				done <- outcome{result, err}
			}()
			var release sync.Once
			t.Cleanup(func() {
				release.Do(func() { close(held.release) })
				select {
				case <-finished:
				case <-time.After(15 * time.Second):
					t.Error("candidate validation goroutine did not stop")
				}
			})
			select {
			case <-held.entered:
			case result := <-done:
				t.Fatalf("candidate was skipped or failed before its sidecar: %+v", result)
			case <-time.After(15 * time.Second):
				t.Fatal("candidate sidecar read was never reached")
			}
			if err := checkLive(); err != nil {
				t.Error(err)
			}
			beforeCalls := secretCalls.Load()
			release.Do(func() { close(held.release) })
			var got outcome
			select {
			case got = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("candidate validation did not finish")
			}
			if secretCalls.Load() != beforeCalls {
				t.Error("validation resolved live provider credentials")
			}
			if got.err != nil {
				t.Fatalf("analysis: %v", got.err)
			}
			if !reflect.DeepEqual(got.result.Mounted, []string{"candidateupdate"}) {
				t.Fatalf("replacement missing: %+v", got.result)
			}
			if invalid {
				found := false
				for _, problem := range got.result.Diagnostics {
					found = found || strings.Contains(problem.Message, "undeclared")
				}
				if !found {
					t.Fatalf("candidate template error not validated: %+v", got.result.Diagnostics)
				}
			} else if len(got.result.Diagnostics) != 0 {
				t.Fatalf("valid candidate refused: %+v", got.result.Diagnostics)
			}
			if err := checkLive(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestPackageCandidateUsesItsOwnNamespacePins(t *testing.T) {
	mountCandidateRuntimeDomain(t, "candidatepin", withLanguageLine(fstest.MapFS{
		"namespace.pin": {Data: []byte("installedpin")},
	}))
	t.Cleanup(func() { memqldsl.UnregisterTree("candidatepin") })
	candidate := withLanguageLines(fstest.MapFS{
		"candidatepin/namespace.pin": {Data: []byte("replacementpin")},
		"candidatepin/queries.memql": {Data: []byte("query widget candidatePinnedWidgets { }\n")},
		"candidatepin/shapes.memql":  {Data: []byte("@row\nshape widget candidatePinnedWidget { }\n")},
	})
	concepts := memoryNodes.NewRegistry(map[string]*memoryNodes.Concept{
		"v1:installedpin:widget":   {Name: "v1:installedpin:widget"},
		"v1:replacementpin:widget": {Name: "v1:replacementpin:widget"},
	})
	functions, shapes, report := newFunctionRegistry(), newShapeRegistry(), newLoadReport()
	if n, _, err := loadUnifiedFunctionsFromTree(candidate, discardLogger(), functions, concepts, report); err != nil || n != 1 {
		t.Fatalf("candidate function load: n=%d err=%v skipped=%+v", n, err, report.Skipped)
	}
	fn, ok := functions.Lookup("candidatePinnedWidgets")
	if !ok || fn.BoundConcept != "v1:replacementpin:widget" {
		t.Fatalf("candidate query used the live namespace pin: %+v", fn)
	}
	if n, err := loadUnifiedShapesFromTree(candidate, discardLogger(), shapes, report); err != nil || n != 1 {
		t.Fatalf("candidate shape load: n=%d err=%v skipped=%+v", n, err, report.Skipped)
	}
	shape, ok := shapes.Get("candidatePinnedWidget")
	if !ok {
		t.Fatal("candidate shape missing")
	}
	bound, err := resolveShapeBoundConcept(concepts, shape)
	if err != nil || bound.Name != "v1:replacementpin:widget" {
		t.Fatalf("candidate shape used the live namespace pin: %+v, %v", bound, err)
	}
	if got := declaredNamespaceForDomain("candidatepin"); got != "installedpin" {
		t.Fatalf("candidate changed the live namespace pin: %q", got)
	}
}

func TestPackageCandidateDoesNotCountDeprecatedFormsAsRuntimeUse(t *testing.T) {
	restore := deprecation.SetCurrent("0.23.0")
	defer restore()
	before := metrics.DSLDeprecatedUsesValue(deprecation.ArrayType)
	result, err := AnalyzePackageDSL(discardLogger(), withLanguageLines(fstest.MapFS{
		"candidatelegacy/concepts.memql": {Data: []byte(deprecatedTicket)},
	}))
	if err != nil || len(result.Diagnostics) != 0 || !reflect.DeepEqual(result.Mounted, []string{"candidatelegacy"}) {
		t.Fatalf("candidate within the language window must load: %+v, %v", result, err)
	}
	if after := metrics.DSLDeprecatedUsesValue(deprecation.ArrayType); after != before {
		t.Fatalf("validation counted a candidate as runtime use: before=%v after=%v", before, after)
	}
}

// Mount through the real boot path, which records runtime ownership.
func mountCandidateRuntimeDomain(t *testing.T, domain string, files fstest.MapFS) {
	t.Helper()
	root := t.TempDir()
	for name, file := range files {
		target := filepath.Join(root, domain, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MEMQL_DSL_PATH", root)
	if got := memqldsl.MountRuntimeDomainsFromEnv(nil); !reflect.DeepEqual(got, []string{domain}) {
		t.Fatalf("runtime mount: %v", got)
	}
}
