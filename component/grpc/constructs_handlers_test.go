package memql

// constructs_handlers_test.go -- the SHAPE-PARITY gate between the construct
// catalog's argument form and the language server's (memql#3749, design §9).
//
// Both surfaces generate the same argument form for the same construct: the
// editor builds one from `memql/runnableConstructs` when the .memql file is
// open, and from ListConstructs when browsing a cluster where it is not. If the
// two shapes drift, the generated form disagrees with the compiler about what
// the construct accepts, and the developer finds out by RUNNING THE WRONG
// THING. A structural test is the only kind that catches a field added to one
// side and not the other, because nothing else in the tree reads both.

import (
	"reflect"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// memqlengineCatalogEntryFixture is one fully-populated catalog entry, so the
// projection test can tell a dropped field from a zero value.
func memqlengineCatalogEntryFixture() memqlengine.ConstructCatalogEntry {
	return memqlengine.ConstructCatalogEntry{
		Name:         "spaceParticipants",
		Kind:         memqlengine.ConstructKindQuery,
		Namespace:    "cognition",
		Origin:       memqlengine.ConstructOriginCore,
		OriginPath:   "cognition/queries.memql",
		Description:  "Get space participants",
		Runnable:     true,
		BoundConcept: "v1:cognition:participant",
		SourceHash:   "0b5f",
		Source:       "query participant spaceParticipants { }",
		Args: []sense.RunnableArg{{
			Name:         "spaceId",
			Type:         "string",
			Required:     true,
			Enum:         []string{"a", "b"},
			Description:  "The space",
			AutoInjected: true,
		}},
	}
}

// constructArgFieldMapping is the wire name for each field of the language
// server's RunnableArg. The one rename is deliberate and documented on the
// proto field: `enum` is a reserved word in the proto language.
var constructArgFieldMapping = map[string]string{
	"Name":         "Name",
	"Type":         "Type",
	"Required":     "Required",
	"Enum":         "EnumValues",
	"Description":  "Description",
	"AutoInjected": "AutoInjected",
}

// TestConstructArgShapeMatchesLanguageServer: every field the language server
// reports for an argument is carried by the wire message, with the same Go
// type, and the wire message carries nothing extra.
func TestConstructArgShapeMatchesLanguageServer(t *testing.T) {
	lsp := reflect.TypeOf(sense.RunnableArg{})
	wire := reflect.TypeOf(memqlv1.ConstructArg{})

	for i := 0; i < lsp.NumField(); i++ {
		f := lsp.Field(i)
		if !f.IsExported() {
			continue
		}
		wireName, mapped := constructArgFieldMapping[f.Name]
		if !mapped {
			t.Errorf("sense.RunnableArg gained field %q with no wire counterpart; add it to ConstructArg and to constructArgFieldMapping, or the two argument forms have diverged", f.Name)
			continue
		}
		wf, ok := wire.FieldByName(wireName)
		if !ok {
			t.Errorf("ConstructArg is missing %q (the wire name for RunnableArg.%s)", wireName, f.Name)
			continue
		}
		if wf.Type != f.Type {
			t.Errorf("field %s: language server has %s, wire has %s", f.Name, f.Type, wf.Type)
		}
	}

	// And nothing extra on the wire: a field ListConstructs reports that the
	// language server does not is a form the editor can only build one way.
	wireNames := map[string]bool{}
	for _, wireName := range constructArgFieldMapping {
		wireNames[wireName] = true
	}
	for i := 0; i < wire.NumField(); i++ {
		f := wire.Field(i)
		if !f.IsExported() || isProtobufInternalField(f) {
			continue
		}
		if !wireNames[f.Name] {
			t.Errorf("ConstructArg carries %q, which the language server does not report; the two argument forms have diverged", f.Name)
		}
	}
}

// isProtobufInternalField reports the generated bookkeeping fields, which are
// exported on the struct but are not part of the message's shape.
func isProtobufInternalField(f reflect.StructField) bool {
	switch f.Name {
	case "state", "sizeCache", "unknownFields":
		return true
	}
	return f.Tag.Get("protobuf") == "" && f.Tag.Get("protobuf_oneof") == ""
}

// TestToConstructInfoProjectsEveryField: the adapter carries every field of a
// catalog entry through, so a field added to the catalog cannot be silently
// dropped at the wire seam.
func TestToConstructInfoProjectsEveryField(t *testing.T) {
	entry := memqlengineCatalogEntryFixture()
	got := toConstructInfo(entry)

	if got.GetName() != entry.Name ||
		got.GetKind() != entry.Kind ||
		got.GetNamespace() != entry.Namespace ||
		got.GetOrigin() != entry.Origin ||
		got.GetOriginPath() != entry.OriginPath ||
		got.GetDescription() != entry.Description ||
		got.GetRunnable() != entry.Runnable ||
		got.GetBoundConcept() != entry.BoundConcept ||
		got.GetSourceHash() != entry.SourceHash ||
		got.GetSource() != entry.Source {
		t.Fatalf("scalar field lost in projection: %+v -> %+v", entry, got)
	}
	if len(got.GetArgs()) != len(entry.Args) {
		t.Fatalf("arg count: %d -> %d", len(entry.Args), len(got.GetArgs()))
	}
	a, w := entry.Args[0], got.GetArgs()[0]
	if w.GetName() != a.Name || w.GetType() != a.Type || w.GetRequired() != a.Required ||
		w.GetDescription() != a.Description || w.GetAutoInjected() != a.AutoInjected ||
		!reflect.DeepEqual(w.GetEnumValues(), a.Enum) {
		t.Errorf("arg field lost in projection: %+v -> %+v", a, w)
	}
}
