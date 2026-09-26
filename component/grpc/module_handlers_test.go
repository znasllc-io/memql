package memql

// module_handlers_test.go -- shape parity between the engine's module rows
// and the wire messages (epic memql#4183), in the same structural style as
// constructs_handlers_test.go: a field added to one side and not the other
// is exactly the drift nothing else in the tree reads both sides to catch.

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// moduleRowFieldMapping is the wire name for each engine ModuleRow field.
var moduleRowFieldMapping = map[string]string{
	"Kind":          "Kind",
	"Name":          "Name",
	"Description":   "Description",
	"State":         "State",
	"StateDetail":   "StateDetail",
	"Scope":         "Scope",
	"EnvComponents": "EnvComponents",
	"FqnPrefixes":   "FqnPrefixes",
	"CodeReference": "CodeReference",
	"MayFlip":       "MayFlip",
}

// moduleEnvVarFieldMapping likewise for the env surface.
var moduleEnvVarFieldMapping = map[string]string{
	"Name":         "Name",
	"Description":  "Description",
	"Secret":       "Secret",
	"Scope":        "Scope",
	"RequiredFor":  "RequiredFor",
	"Set":          "Set",
	"Value":        "Value",
	"DefaultValue": "DefaultValue",
}

func assertShapeParity(t *testing.T, engineType, wireType reflect.Type, mapping map[string]string) {
	t.Helper()
	for i := 0; i < engineType.NumField(); i++ {
		f := engineType.Field(i)
		if !f.IsExported() {
			continue
		}
		wireName, mapped := mapping[f.Name]
		if !mapped {
			t.Errorf("%s gained field %q with no wire counterpart; extend %s and the mapping",
				engineType.Name(), f.Name, wireType.Name())
			continue
		}
		wf, ok := wireType.FieldByName(wireName)
		if !ok {
			t.Errorf("%s is missing %q (the wire name for %s.%s)",
				wireType.Name(), wireName, engineType.Name(), f.Name)
			continue
		}
		if wf.Type != f.Type {
			t.Errorf("field %s: engine has %s, wire has %s", f.Name, f.Type, wf.Type)
		}
	}
}

func TestModuleRowShapeMatchesWire(t *testing.T) {
	assertShapeParity(t,
		reflect.TypeOf(memqlengine.ModuleRow{}),
		reflect.TypeOf(memqlv1.ModuleInfo{}),
		moduleRowFieldMapping)
}

func TestModuleEnvVarShapeMatchesWire(t *testing.T) {
	assertShapeParity(t,
		reflect.TypeOf(memqlengine.ModuleEnvVar{}),
		reflect.TypeOf(memqlv1.ModuleEnvVar{}),
		moduleEnvVarFieldMapping)
}

// TestModuleInfoToProtoCarriesEveryField: a fully-populated row survives the
// mapping, so a zero value cannot masquerade as a mapped one.
func TestModuleInfoToProtoCarriesEveryField(t *testing.T) {
	row := memqlengine.ModuleRow{
		Kind:          memqlengine.ModuleKindPack,
		Name:          "harness",
		Description:   "d",
		State:         "enabled",
		StateDetail:   "loaded on this node",
		Scope:         memqlengine.ModuleScopeCluster,
		EnvComponents: []string{"engine"},
		FqnPrefixes:   []string{"integration.harnessRecall."},
		CodeReference: "service:memql",
		MayFlip:       true,
	}
	p := moduleInfoToProto(row)
	if p.GetKind() != row.Kind || p.GetName() != row.Name || p.GetDescription() != row.Description ||
		p.GetState() != row.State || p.GetStateDetail() != row.StateDetail || p.GetScope() != row.Scope ||
		len(p.GetEnvComponents()) != 1 || len(p.GetFqnPrefixes()) != 1 ||
		p.GetCodeReference() != row.CodeReference || p.GetMayFlip() != row.MayFlip {
		t.Fatalf("moduleInfoToProto dropped a field: %+v -> %+v", row, p)
	}
}

// TestSetPackEnabledAuditsEveryAttemptOnceWithTheActorsRole: moving the write
// into component/memql must not cost the handler its contract -- exactly one
// audit event per attempt, refusals included, naming who tried. The refused
// paths need no database, so they run everywhere; the admitted write is
// component/memql's pack_flip_db_test.go.
func TestSetPackEnabledAuditsEveryAttemptOnceWithTheActorsRole(t *testing.T) {
	cases := []struct {
		name string
		role auth.Role
		pack string
		code int32
	}{
		{"a developer on a pack that is not a storefront pack", auth.RoleDeveloper, "referencepack", 7},
		{"an admin", auth.RoleAdmin, "referencepack", 7},
		{"an owner naming no registered pack", auth.RoleOwner, "no-such-pack", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newCaptureStream(t)
			sink := &recordingAudit{}
			svc := &service{
				logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				engine:      &memqlengine.MemQLEngine{},
				moduleAudit: sink,
			}
			s := &streamSession{
				service: svc,
				stream:  cs,
				logger:  svc.logger,
				access:  &auth.AccessContext{UserId: "u1", PrimaryEmail: "p@example.com", Role: tc.role},
			}
			s.accessLoaded = true

			env := &memqlv1.MemqlClientMessage{MessageId: "m1"}
			if err := s.handleSetPackEnabled(env, &memqlv1.SetPackEnabledMsg{RequestId: "r1", PackDomain: tc.pack, Enabled: true}); err != nil {
				t.Fatalf("the handler returned an error, which would tear the stream down: %v", err)
			}
			res := cs.lastSent().GetSetPackEnabledResult()
			if res == nil || res.GetErrorCode() != tc.code {
				t.Fatalf("reply = %+v, want error code %d", res, tc.code)
			}
			events := sink.all()
			if len(events) != 1 {
				t.Fatalf("%d audit events for one attempt, want exactly 1", len(events))
			}
			ev := events[0]
			if ev.Outcome != identity.AuditOutcomeBlocked || ev.ActorRole != string(tc.role) || ev.ActorUserId != "u1" || ev.TargetId != tc.pack {
				t.Fatalf("audit event = %+v, want a blocked event naming u1 as %s on %s", ev, tc.role, tc.pack)
			}
		})
	}
}
