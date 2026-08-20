package memql

// module_handlers_test.go -- shape parity between the engine's module rows
// and the wire messages (epic memql#4183), in the same structural style as
// constructs_handlers_test.go: a field added to one side and not the other
// is exactly the drift nothing else in the tree reads both sides to catch.

import (
	"reflect"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
	}
	p := moduleInfoToProto(row)
	if p.GetKind() != row.Kind || p.GetName() != row.Name || p.GetDescription() != row.Description ||
		p.GetState() != row.State || p.GetStateDetail() != row.StateDetail || p.GetScope() != row.Scope ||
		len(p.GetEnvComponents()) != 1 || len(p.GetFqnPrefixes()) != 1 ||
		p.GetCodeReference() != row.CodeReference {
		t.Fatalf("moduleInfoToProto dropped a field: %+v -> %+v", row, p)
	}
}
