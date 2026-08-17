package scan

import (
	"slices"
	"strings"
	"testing"
)

// memql#3834 step 3: the two shapes the issue names -- NAME TABLES and INJECTED
// GETTERS -- and the ambiguity rule that keeps both honest.
//
// Both were previously detected as residual (a site, no key) or as nothing at
// all, and between them they hid ~25 live operator knobs: the whole
// MEMQL_REALTIME_* family, MEMQL_VOICE_EXECUTOR, MEMQL_AVATAR_VENDOR,
// MEMQL_GRPC_ADDR -- and, through the struct-field half, every SERVER_* the
// engine's HTTP server reads on every node (memql#3892).

const step3Mod = "module github.com/znasllc-io/memql\n\ngo 1.26.1\n"

// ---------------------------------------------------------------------------
// name tables: a struct of key constants, and a prefix held in a field
// ---------------------------------------------------------------------------

// The live shape, reduced: a package-level key table, a loader struct carrying
// the prefix, and reads that name neither directly.
func TestStructFieldNameTableResolves(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

const svcPrefix = "MEMQL_SVC"

type Keys struct {
	Address string
	Timeout string
}

type Loader struct {
	Prefix string
	Keys   Keys
}

var defaultKeys = Keys{
	Address: "ADDRESS",
	Timeout: "TIMEOUT_MS",
}

func LoadDefaults() { load(Loader{Prefix: svcPrefix}) }

func load(loader Loader) {
	keys := loader.Keys
	if keys == (Keys{}) {
		keys = defaultKeys
	}
	reader := env.NewEnvReader(loader.Prefix)
	_, _ = reader.String(keys.Address)
	_, _ = reader.OptionalInt(keys.Timeout)
}
`
	out := scanStep3(t, src)
	want := []string{"MEMQL_SVC_ADDRESS", "MEMQL_SVC_TIMEOUT_MS"}
	if got := keysOf(out); !slices.Equal(got, want) {
		t.Errorf("reads = %v, want %v.\n"+
			"Neither name exists as a literal anywhere: the prefix is a struct FIELD and each key is "+
			"a field of a second struct. That is exactly why SERVER_ADDRESS was read by every node "+
			"and reported by nothing (memql#3892).", got, want)
	}
}

// AMBIGUITY IS ABSENCE. Keying the table on the field NAME rather than on the
// declaring type is what makes this pass possible without go/types -- and this
// is what keeps it honest rather than lucky. Two literals in one package that
// disagree about a field resolve to NEITHER, so the failure mode is a site that
// stays in the residual, never a confidently wrong key.
func TestStructFieldAmbiguityResolvesToNothing(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

type A struct{ Address string }
type B struct{ Address string }

var one = A{Address: "ADDRESS_ONE"}
var two = B{Address: "ADDRESS_TWO"}

func load(k A) {
	reader := env.NewEnvReader("MEMQL_SVC")
	_, _ = reader.String(k.Address)
}
`
	out := scanStep3(t, src)
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none. Two literals disagree about `Address`, so no single value "+
			"is correct and a guess would register a name nothing sets.", got)
	}
	if len(out.Unresolvable) != 1 {
		t.Fatalf("residual = %d, want 1: an ambiguous field must stay VISIBLE in the residual rather "+
			"than vanish. Got %v", len(out.Unresolvable), out.Unresolvable)
	}
}

// A residual reader site must now say WHICH HALF is missing. After this pass the
// suffix usually folds and it is the prefix that does not, so the old blanket
// "neither the prefix nor the suffix is resolved" sent a reader to the wrong
// place (memql#3834).
func TestResidualNamesWhichHalfIsMissing(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

type Keys struct{ DSN string }

var defaultKeys = Keys{DSN: "DSN"}

func load(prefix string) {
	reader := env.NewEnvReader(prefix)
	_, _ = reader.String(defaultKeys.DSN)
}
`
	out := scanStep3(t, src)
	if len(out.Unresolvable) != 1 {
		t.Fatalf("residual = %d, want 1: %v", len(out.Unresolvable), out.Unresolvable)
	}
	why := out.Unresolvable[0].Why
	if !strings.Contains(why, `"DSN"`) {
		t.Errorf("residual does not report the suffix it DID resolve, so a reader cannot tell this "+
			"apart from a site where nothing resolved: %q", why)
	}
	if !strings.Contains(why, "prefix") {
		t.Errorf("residual does not say the prefix is the missing half: %q", why)
	}
}

// ---------------------------------------------------------------------------
// injected getters: a local closure over a caller-supplied reader
// ---------------------------------------------------------------------------

// integrations/voice/agent/config.go, reduced. The keys were STRING LITERALS
// the whole time; what was invisible was that `get` reads the environment,
// because the reader is injected for testability and the helper is a closure
// rather than a named function.
func TestInjectedGetterClosureResolves(t *testing.T) {
	const src = `package x

import "os"

type Getenv func(key string) string

func LoadConfig(getenv Getenv) {
	if getenv == nil {
		getenv = os.Getenv
	}
	get := func(key, def string) string {
		v := getenv(key)
		if v == "" {
			return def
		}
		return v
	}
	getInt := func(key string, def int) int {
		_ = getenv(key)
		return def
	}
	_ = get("MEMQL_REALTIME_MODEL", "gpt-realtime-2")
	_ = getInt("MEMQL_REALTIME_MAX_SESSION_SEC", 1800)
}
`
	out := scanStep3(t, src)
	want := []string{"MEMQL_REALTIME_MAX_SESSION_SEC", "MEMQL_REALTIME_MODEL"}
	if got := keysOf(out); !slices.Equal(got, want) {
		t.Errorf("reads = %v, want %v.\n"+
			"The key is at index 0 for `get` and index 0 for `getInt`, and both closures reach the "+
			"environment only through the injected `getenv`.", got, want)
	}
}

// The key is not always the first argument, so the parameter INDEX is what gets
// recorded -- the same property the package-level helper pass carries.
func TestInjectedGetterHonoursTheParameterIndex(t *testing.T) {
	const src = `package x

import "os"

func Load() {
	getenv := os.Getenv
	get := func(def, key string) string {
		v := getenv(key)
		if v == "" {
			return def
		}
		return v
	}
	_ = get("a-default", "MEMQL_SECOND_ARG_KEY")
}
`
	out := scanStep3(t, src)
	want := []string{"MEMQL_SECOND_ARG_KEY"}
	if got := keysOf(out); !slices.Equal(got, want) {
		t.Errorf("reads = %v, want %v -- the key is the SECOND parameter, and taking argument 0 "+
			"would register the default string as an env key", got, want)
	}
}

// STRICTNESS, mirroring collectEnvKeyHelpers. A closure that never reaches an
// env reader is not a helper, and claiming it would register whatever SCREAMING
// _SNAKE string its callers happen to pass.
func TestClosureThatReadsNoEnvIsNotAHelper(t *testing.T) {
	const src = `package x

import "os"

func Load() {
	getenv := os.Getenv
	_ = getenv
	label := func(key, def string) string {
		if key == "" {
			return def
		}
		return key
	}
	_ = label("NOT_AN_ENV_KEY", "x")
}
`
	out := scanStep3(t, src)
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none: `label` never reaches the environment, so its argument is "+
			"not an env key", got)
	}
}

// A shadowing local of the parameter's name means the identifier reaching the
// reader may not be the parameter -- so the closure is not claimed.
func TestShadowedParameterDisqualifiesTheClosure(t *testing.T) {
	const src = `package x

import "os"

func Load() {
	getenv := os.Getenv
	get := func(key string) string {
		key := computed()
		return getenv(key)
	}
	_ = get("MEMQL_NOT_ACTUALLY_READ")
}

func computed() string { return "OTHER" }
`
	out := scanStep3(t, src)
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none: the parameter is shadowed before it reaches the reader, so "+
			"the call-site argument is not the key", got)
	}
}

// A closure whose reader is a plain function value that was never assigned
// os.Getenv reads nothing this pass can vouch for.
func TestClosureOverAnUnknownReaderIsNotAHelper(t *testing.T) {
	const src = `package x

func Load(fetch func(string) string) {
	get := func(key string) string { return fetch(key) }
	_ = get("MEMQL_UNKNOWN_SOURCE")
}
`
	out := scanStep3(t, src)
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none: `fetch` is never shown to read the environment, so treating "+
			"its key as an env var would be a guess", got)
	}
}

func scanStep3(t *testing.T, src string) Outcome {
	t.Helper()
	root := writeGoFixture(t, map[string]string{
		"app/x.go": src,
		"go.mod":   step3Mod,
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	return out
}
