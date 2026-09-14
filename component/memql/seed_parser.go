package memql

// seed_parser.go holds the in-package seed declaration: the generic field
// tree seedDeclASTToInternal (seed_converter.go) fills from the shared
// parser's *ast.SeedDecl and compileSeedDecl (seeds.go) lowers. The
// hand-rolled parser that used to live here -- with its own seed annotation
// allow-list -- was unreferenced from production once seeds moved onto the
// shared parser (memql#335); it is deleted with memql#5359, which makes the
// annotation registry the one list of what a seed takes.

// seedDecl is the parsed top-level seed declaration. The body is
// represented as a generic seedBlock tree -- the loader validates
// it against the bound concept's schema.
type seedDecl struct {
	// Annotation values
	name         string // declaration name (the `seed XXX` identifier)
	description  string // @description value
	namespace    string // @namespace value
	version      string // @version value
	scope        string // @scope value: "global" | "perUser" (empty => default policy applied by loader)
	templateFile string // @templateFile path (optional; e.g. systemPrompt source for agent seeds)

	// Use clause -- binds the file's target concept by namespace.concept name.
	// Loader resolves this to the canonical concept id (e.g. v1:agents:agent).
	// Populated by either the legacy file-top `use <ns>.<concept>` form OR
	// by the canonical `seed <Concept> <name>` signature shape (in which
	// case signatureConcept is also set and useNamespace stays empty --
	// the loader resolves the concept via the file's Form B imports).
	useNamespace     string
	useConcept       string
	signatureConcept string       // set when concept binding came from signature
	imports          []seedImport // Form B file-top imports

	// Body -- generic field/block tree. Field validation happens at
	// load time against the bound concept's schema.
	body seedBlock
}

// seedImport records a single Form B file-top `use module.{ names }`
// import. The loader uses these to resolve a signature-bound concept
// name to its source module.
type seedImport struct {
	module string
	names  []string
}

// seedBlock is one `{ ... }` worth of field assignments + nested
// blocks. Maps preserve declaration-order via the keys slice so we
// can render deterministic error messages.
type seedBlock struct {
	keys   []string             // insertion order
	fields map[string]seedValue // key -> value
	nested map[string]seedBlock // key -> nested block
}

// seedValue is a typed scalar or scalar-array from a body field
// assignment. The loader downcasts to the concept's declared type
// for that field.
type seedValue struct {
	kind     seedValueKind
	str      string   // when kind == seedString
	intV     int64    // when kind == seedInt
	floatV   float64  // when kind == seedFloat
	boolV    bool     // when kind == seedBool
	stringsV []string // when kind == seedStringArray
}

type seedValueKind int

const (
	seedNone seedValueKind = iota
	seedString
	seedInt
	seedFloat
	seedBool
	seedStringArray
)

func newSeedBlock() seedBlock {
	return seedBlock{
		fields: make(map[string]seedValue),
		nested: make(map[string]seedBlock),
	}
}
