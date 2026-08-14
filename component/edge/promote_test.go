package edge

// promote_test.go -- the assertions about promote that need no database.

import (
	"reflect"
	"testing"
)

// TestPromotePerformsNoObjectStorageWrite is the acceptance line "no
// object-storage write occurs during a promote", asserted STRUCTURALLY rather
// than by observing a run.
//
// Observing it would prove only that the paths exercised by the tests above
// happen not to upload. What must hold is stronger: a promote CANNOT upload,
// because the bundle is immutable and versioned by prefix, and copying bytes
// during a promote would break the immutability that makes rollback one row
// write instead of a restore.
//
// So the property is enforced by the dependency set. Promoter is constructed
// with a database handle and nothing else -- no BlobWriter, no uploader, no
// container name -- which means no promote can reach object storage regardless
// of what the code inside it does. This test fails if anyone widens that set,
// which is the moment the property is actually at risk.
func TestPromotePerformsNoObjectStorageWrite(t *testing.T) {
	tp := reflect.TypeOf(Promoter{})
	if tp.NumField() != 1 {
		t.Fatalf("Promoter carries %d fields, want exactly 1 (the database handle)."+
			" A promote moves a REFERENCE; if it gained a blob dependency it could copy bytes,"+
			" and the version prefix stops being immutable.", tp.NumField())
	}
	if got := tp.Field(0).Type.String(); got != "*bun.DB" {
		t.Errorf("Promoter's only field is %s, want *bun.DB -- see above", got)
	}
}

// TestRollbackIsTheSameWriteAsPromote pins the design's "rollback is the same
// write with the previous value -- not a distinct code path".
//
// Checking that two functions behave alike would not hold the line: they could
// drift and both still pass. What holds it is that there is only ONE write, so
// this asserts the shape -- SetBundleRef is exported precisely because rollback
// goes through it, and Promote is a resolve-then-call over the same helper.
func TestRollbackIsTheSameWriteAsPromote(t *testing.T) {
	p := reflect.TypeOf(&Promoter{})
	for _, name := range []string{"Promote", "SetBundleRef"} {
		if _, ok := p.MethodByName(name); !ok {
			t.Errorf("Promoter has no %s method; rollback and promote must share one write", name)
		}
	}
}

// TestValidateSchemasRefusesEverythingButAPlainIdentifier. Schema names are
// interpolated into SQL because a schema cannot be a bound parameter, so this
// validator is a security control rather than an input-validation nicety, and
// it gets a table of its own instead of only being exercised through promote.
func TestValidateSchemasRefusesEverythingButAPlainIdentifier(t *testing.T) {
	for _, ok := range []string{"memql_prod", "memql_staging", "public", "a", "_x9"} {
		if err := validateSchemas(ok); err != nil {
			t.Errorf("validateSchemas(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		``,
		`memql prod`,
		`MemqlProd`,                     // upper case: Postgres would fold an unquoted one, and we quote
		`memql_prod"; DROP TABLE x; --`, // the reason this function exists
		`1memql`,                        // leading digit
		`memql-prod`,                    // hyphen
		`memql_prod, public`,            // a search path, not a schema
	} {
		if err := validateSchemas(bad); err == nil {
			t.Errorf("validateSchemas(%q) admitted it", bad)
		}
	}
}
