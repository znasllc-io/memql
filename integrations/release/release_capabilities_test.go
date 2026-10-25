package release

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

func TestReleaseIntegrationRegistersLoadedExecutors(t *testing.T) {
	eng := realEngine(t)
	// These declarations must be loaded, not invented by the test. The audit
	// below checks every enabled release executor in the actual embedded DSL.
	for _, name := range []string{"releaseOverlayNativeEvidence", "releaseOverlayBindPublication"} {
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil || !fn.IsBuiltin() || !fn.Enabled || fn.Executor != "integration.release."+name {
			t.Fatalf("missing enabled embedded declaration %s: %v", name, err)
		}
	}
	if err := eng.RegisterIntegration(NewIntegration(nil, eng, resolver{})); err != nil {
		t.Fatal(err)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("release startup executor inventory: %v", err)
	}
}

func TestReleaseOverlayRegisteredPortsRefuseWithoutNativeScope(t *testing.T) {
	integration := NewIntegration(nil, nil, resolver{})
	seen := map[string]bool{}
	for _, capability := range integration.Capabilities() {
		if !strings.HasPrefix(capability.Name, "releaseOverlay") {
			continue
		}
		seen[capability.Name] = true
		for _, caller := range []struct {
			name string
			ctx  context.Context
		}{
			{"anonymous", context.Background()},
			{"owner", ownerCtx()},
			{"internal_owner", auth.ContextWithInternalOrigin(ownerCtx())},
		} {
			t.Run(capability.Name+"/"+caller.name, func(t *testing.T) {
				rows, err := capability.Handler(caller.ctx, map[string]any{
					"approved": true, "verified": true, "formatVersion": 3,
					"candidateId": "candidate", "catalogDigest": "sha256:" + strings.Repeat("a", 64),
				}, 0)
				if err == nil || !strings.Contains(err.Error(), "requires its authorized workflow scope") || len(rows) != 0 {
					t.Fatalf("direct caller acquired native port: rows=%d err=%v", len(rows), err)
				}
			})
		}
	}
	for _, name := range []string{"releaseOverlayNativeEvidence", "releaseOverlayBindPublication"} {
		if !seen[name] {
			t.Errorf("declared overlay port was not registered: %s", name)
		}
	}
}

func TestReleaseOverlayRuntimePortsRemainVersionScoped(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprintf("format_%d", version), func(t *testing.T) {
			scope := &overlayScope{configuration: overlayConfiguration{FormatVersion: version}}
			operations := scope.operations()
			for _, name := range []string{"releaseOverlayRead", "releaseOverlayBuild", "releaseOverlayReserve", "releaseOverlayPropose", "releaseOverlayObserve"} {
				if operations[name] == nil {
					t.Fatalf("existing scoped port disappeared: %s", name)
				}
			}
			for _, name := range []string{"releaseOverlayNativeEvidence", "releaseOverlayBindPublication"} {
				if (operations[name] != nil) != (version == 3) {
					t.Fatalf("native evidence scope widened: format=%d operation=%s", version, name)
				}
				// The actual workflow capture must reject either v3 operation in
				// a legacy scope before it can run any callback.
				source := "@template\nautomation overlayPortProbe {\n builtin " + name + "()\n}\n"
				if name == "releaseOverlayBindPublication" {
					source = "@template\nautomation overlayPortProbe {\n builtin " + name + "(candidateId: \"candidate\", catalogDigest: \"digest\")\n}\n"
				}
				_, err := workflowhost.Capture("overlayPortProbe", "overlay-port-test", func(kind, requested string) (string, error) {
					if kind != "automation" || requested != "overlayPortProbe" {
						return "", fmt.Errorf("unexpected source %s:%s", kind, requested)
					}
					return source, nil
				}, operations)
				if (err == nil) != (version == 3) {
					t.Fatalf("workflow capture scope: format=%d operation=%s err=%v", version, name, err)
				}
				if version != 3 && !strings.Contains(err.Error(), "has no scoped operation") {
					t.Fatalf("legacy capture failed for an unrelated reason: %v", err)
				}
			}
		})
	}
}
