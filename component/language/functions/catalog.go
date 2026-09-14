// Package functions is the function catalog of D10 in
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md:
// every function and method a v1 expression can call, one entry each, one
// spelling each, with its signature, the tier it runs in and a short
// description. It is meant to be the one list: whatever needs to know what a
// call means -- the load-time type check, the two evaluators, Sense, the
// generated docs -- reads this table rather than keeping a list of its own.
//
// # What is not here
//
//   - Operators. A retired function whose meaning is now an operator --
//     `cond(p, a, b)` is `p ? a : b`, `concat(a, b)` is `a + b` -- has no entry;
//     RetiredFunctions maps its name to the operator form, so Sense and the docs
//     can say what to write instead.
//   - Predicates. Applying a spec or a trait (`isActiveRecord(row)`) reads like
//     a call, but a predicate is a construct the author declared, resolved from
//     the construct registry. Where one may be applied is a position rule
//     (tiers.PredicateAdmission), not a catalog entry.
//   - Construct calls (`query activeUsers(...)`), which name a construct too.
//   - The reserved roots (`args`, `actor`, `now`, `config`, `event`, ...).
//     They are names, not calls: the retired `now()` call form is in
//     RetiredFunctions.
//
// # Tiers
//
// A P function lowers to SQL: given an input that reads the row, the lowering
// turns the call into SQL the query pushes down. An M function is evaluated in
// process by EvalExpr. An M function is still usable in a pushdown position on
// inputs that do not read the row (`row.expiresAt < addDuration(now, "P1D")`):
// such an input is a plan constant, evaluated once per call before the query
// runs and bound as a parameter. component/language/tiers states which
// positions admit which tier.
package functions

import (
	"sort"
	"strings"
)

// Tier is where a function runs: pushed down to SQL (P) or in process (M).
type Tier string

const (
	// TierP functions lower to SQL when their input reads the row.
	TierP Tier = "P"
	// TierM functions are evaluated in process; in a pushdown position only
	// on plan-constant inputs.
	TierM Tier = "M"
)

// The type words a Param's Type and a Function's Returns are spelled in. They
// are the vocabulary of the load-time type check and of signature help. A
// method's Receiver is a type word too: TypeString, TypeList, or TypeAny for a
// method every receiver answers to.
const (
	TypeString = "string"
	TypeNumber = "number"
	TypeBool   = "bool"
	// TypeDatetime is an RFC 3339 timestamp, carried as a string.
	TypeDatetime = "datetime"
	// TypeDuration is an ISO 8601 duration such as "P1D" or "-PT2H", carried
	// as a string.
	TypeDuration = "duration"
	TypeList     = "list"
	TypeMap      = "map"
	TypeAny      = "any"
	TypeLambda   = "lambda"
	TypeRow      = "row"
	// TypeRows is a return type only: the row set a relationship traversal
	// yields, which the query it sits in continues from. No expression holds
	// one as a value, so no parameter takes it.
	TypeRows = "rows"
)

// Param is one parameter of a function or a method.
type Param struct {
	// Name is the label signature help shows.
	Name string
	// Type is a type word (TypeString, TypeLambda, ...), never TypeRows.
	Type string
	// Optional marks a parameter a call may leave out. A traversal's label is
	// the one optional parameter that comes FIRST: `references("respondsAs",
	// r => ...)` and `references(r => ...)` are told apart by the argument
	// count, as they are today.
	Optional bool
	// Variadic marks a last parameter that takes any number of arguments.
	Variadic bool
}

// Function is one catalog entry: a function (Receiver "") or a method.
type Function struct {
	// Name is the one spelling. It is case-sensitive: the pre-v1 grammar
	// dispatched on the lowercased name, so `shortid` and `shortId` were one
	// function; in the v1 grammar only `shortId` is.
	Name string
	// Doc is one to three sentences, opening on a verb, saying what the call
	// returns and what an absent input does.
	Doc string
	// Receiver is "" for a function, or the type word of the value a method is
	// called on: TypeString, TypeList, or TypeAny.
	Receiver string
	// Params lists the parameters in call order.
	Params []Param
	// Returns is the type word of the result.
	Returns string
	// Tier is TierP or TierM (see the package comment).
	Tier Tier
	// Retired lists the retired spellings this entry replaces, for Sense and
	// the docs to show as "was:". A spelling whose replacement is an operator
	// rather than an entry is in RetiredFunctions instead.
	Retired []string
}

// Key is the name a function is found by: its Name, or receiver.name for a
// method ("list.any", "string.includes").
func (f Function) Key() string {
	if f.Receiver == "" {
		return f.Name
	}
	return f.Receiver + "." + f.Name
}

// Signature renders the entry for hover and signature help:
// "addDuration(ts datetime, dur duration) datetime", "list.any(pred lambda)
// bool". An optional parameter is written `name? type` and a variadic one
// `name ...type`, the house style dslspec already uses.
func (f Function) Signature() string {
	var b strings.Builder
	b.WriteString(f.Key())
	b.WriteByte('(')
	for i, p := range f.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		if p.Optional {
			b.WriteByte('?')
		}
		b.WriteByte(' ')
		if p.Variadic {
			b.WriteString("...")
		}
		b.WriteString(p.Type)
	}
	b.WriteByte(')')
	if f.Returns != "" {
		b.WriteByte(' ')
		b.WriteString(f.Returns)
	}
	return b.String()
}

// clone copies the slices, so a caller that edits what it was handed cannot
// change the entry the next caller reads.
func (f Function) clone() Function {
	f.Params = append([]Param(nil), f.Params...)
	f.Retired = append([]string(nil), f.Retired...)
	return f
}

// Catalog returns every entry in a stable order: functions first, by name, then
// methods, by receiver and then name. The result is a copy.
func Catalog() []Function {
	out := make([]Function, len(catalog.fns))
	for i, f := range catalog.fns {
		out[i] = f.clone()
	}
	return out
}

// Lookup finds a function (not a method) by its one spelling.
func Lookup(name string) (Function, bool) {
	return catalog.function(name)
}

// Method finds a method by receiver and name. A method declared on TypeAny
// answers for a receiver that has no method of that name of its own.
func Method(receiver, name string) (Function, bool) {
	return catalog.method(receiver, name)
}

// RetiredFunctions maps each retired function, by the name it was called with,
// to what a v1 expression writes instead. Most replacements are operators and
// have no catalog entry; the rest are the method an entry now spells. Sense and
// the docs read it. The parser keeps its own refusal table, and must refuse
// every name here. The result is a copy.
//
// `contains` is deliberately absent: only its two-argument substring form
// retired (string.includes lists it), and the traversal keeps the name.
func RetiredFunctions() map[string]string {
	return map[string]string{
		"cond":     "p ? a : b",
		"concat":   "a + b",
		"coalesce": "a ?? b",
		// exists(x) also read a blank string as absent; the migrator writes
		// `(x != nil && x != "")` at the call sites that relied on it.
		"exists": "x != nil",
		"len":    "x.count()",
		"count":  "x.count()",
		"and":    "a && b",
		"or":     "a || b",
		"not":    "!a",
		"lt":     "a < b",
		"gt":     "a > b",
		"lte":    "a <= b",
		"gte":    "a >= b",
		"first":  "x.first()",
		"last":   "x.last()",
		// mean() was an automation-evaluator operand the tree never used;
		// it retires to the one averaging spelling rather than vanishing
		// into an "unknown function".
		"mean":      "x.avg()",
		"timestamp": "now",
		"now":       "now",
	}
}

// RetiredMethods maps each retired method, by receiver.name, to what a v1
// expression writes instead. Membership is the `in` operator, so the
// collection method `.contains(v)` is gone. The result is a copy.
func RetiredMethods() map[string]string {
	return map[string]string{
		"list.contains": "v in <list>",
	}
}

// table is a catalog with its lookups: the package's one instance holds the
// entries below; tests build their own to pin a lookup rule no entry exercises.
type table struct {
	fns   []Function
	byKey map[string]int
}

func newTable(entries []Function) table {
	fns := append([]Function(nil), entries...)
	sort.SliceStable(fns, func(i, j int) bool {
		a, b := fns[i], fns[j]
		if (a.Receiver == "") != (b.Receiver == "") {
			return a.Receiver == ""
		}
		if a.Receiver != b.Receiver {
			return a.Receiver < b.Receiver
		}
		return a.Name < b.Name
	})
	byKey := make(map[string]int, len(fns))
	for i, f := range fns {
		byKey[f.Key()] = i
	}
	return table{fns: fns, byKey: byKey}
}

func (t table) function(name string) (Function, bool) {
	i, ok := t.byKey[name]
	if !ok || t.fns[i].Receiver != "" {
		return Function{}, false
	}
	return t.fns[i].clone(), true
}

func (t table) method(receiver, name string) (Function, bool) {
	if receiver == "" {
		return Function{}, false
	}
	if i, ok := t.byKey[receiver+"."+name]; ok {
		return t.fns[i].clone(), true
	}
	if i, ok := t.byKey[TypeAny+"."+name]; ok {
		return t.fns[i].clone(), true
	}
	return Function{}, false
}

var catalog = newTable(entries())

// traversal builds a relationship traversal: P, returning rows, with the
// optional leading `as` label and the lambda that selects the rows it starts
// from. Every traversal shares the shape, so the entries below differ only in
// their name and what they follow.
func traversal(name, follows string) Function {
	return Function{
		Name: name,
		Doc: "Returns " + follows + "; a leading label follows only the edges whose `as` label it names. " +
			"A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped.",
		Params: []Param{
			{Name: "label", Type: TypeString, Optional: true},
			{Name: "match", Type: TypeLambda},
		},
		Returns: TypeRows,
		Tier:    TierP,
	}
}

// entries is the catalog. Where each entry comes from:
//
//   - the functions are the pre-v1 grammar's expression builtins
//     (parser.CallableBuiltins) that keep a function spelling, plus var() and
//     error() (grammar accessors) and systemVar() / secret() / systemSecret(),
//     which the mutation-template evaluator resolves in that call form (the
//     automation evaluator spells the four as dotted roots, `secret.NAME`,
//     which no tree expression uses);
//   - the traversals are ast.RelationshipFunction, `contains` included -- its
//     substring form moved to string.includes, and with it the reason it could
//     not take a label;
//   - the list methods are the collection library's dispatch table
//     (component/memql/collection_method.go, evalCollectionMethod) minus
//     `contains`, plus `nodes`, the step-result accessor logic bodies call as
//     `rows.nodes()` (component/automations: stepMethodAccessors). `Ran`, the
//     other step accessor, is a step status rather than a collection method and
//     is not here;
//   - the string methods are new: includes is the substring test that was
//     contains(s, sub), and count is a character count, which the pre-v1
//     grammar had no spelling for (len(x) counted a list and refused a
//     string).
//
// Docs describe what today's evaluators do with an absent input -- an absent
// list reads as empty (toCollection) -- so the two evaluators of this epic have
// one written answer to reproduce.
func entries() []Function {
	return []Function{
		// ---- Functions: strings, ids, time, configuration ----
		{
			Name:    "lower",
			Doc:     "Returns value with its letters lowercased. An absent value yields the empty string.",
			Params:  []Param{{Name: "value", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name:    "upper",
			Doc:     "Returns value with its letters uppercased. An absent value yields the empty string.",
			Params:  []Param{{Name: "value", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name:    "trim",
			Doc:     "Returns value without its leading and trailing whitespace. An absent value yields the empty string.",
			Params:  []Param{{Name: "value", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			// memql#3009: a fixed width on every input is what keeps
			// hash(hash(a) + hash(b)) injective, so an absent input is hashed
			// rather than passed through as "".
			Name: "hash",
			Doc: "Returns the SHA-256 digest of value as 64 lowercase hexadecimal characters. " +
				"An absent value hashes as the empty string, so the result is always 64 characters wide.",
			Params:  []Param{{Name: "value", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			// Strips ONE prefix (memql#2981), so it is not idempotent on a
			// value whose short id is itself canonical.
			Name: "shortId",
			Doc: "Returns the bare short id of value by stripping one canonical concept prefix; a value that is already bare comes back unchanged. " +
				"An absent or blank value yields the empty string.",
			Params:  []Param{{Name: "value", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			// The concept is a string: a bare concept short-name is not a
			// name a v1 expression resolves.
			Name: "canonicalId",
			Doc: "Returns value as a canonical id of the named concept, whether it arrives as a bare short id or already canonical. " +
				"An absent or blank value yields the empty string, and a value canonical for a different concept is an error.",
			Params:  []Param{{Name: "value", Type: TypeString}, {Name: "concept", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name:    "toString",
			Doc:     "Returns value as text; a string comes back unchanged. An absent value yields the empty string.",
			Params:  []Param{{Name: "value", Type: TypeAny}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name: "addDuration",
			Doc: "Returns ts moved by the ISO 8601 duration dur, as an RFC 3339 timestamp; a leading minus sign moves it back. " +
				"An absent or unparseable argument is an error.",
			Params:  []Param{{Name: "ts", Type: TypeDatetime}, {Name: "dur", Type: TypeDuration}},
			Returns: TypeDatetime,
			Tier:    TierM,
		},
		{
			Name: "daysBetween",
			Doc: "Returns the number of whole days from a to b, truncated toward zero and negative when b is earlier. " +
				"An absent or unparseable argument is an error.",
			Params:  []Param{{Name: "a", Type: TypeDatetime}, {Name: "b", Type: TypeDatetime}},
			Returns: TypeNumber,
			Tier:    TierM,
		},
		{
			Name:    "error",
			Doc:     "Raises an error carrying message and ends the evaluation, so it never returns a value.",
			Params:  []Param{{Name: "message", Type: TypeString}},
			Returns: TypeAny,
			Tier:    TierM,
		},
		{
			Name: "var",
			Doc: "Returns the plaintext value of the configuration variable named name, looked up among the partition variables and then the global ones. " +
				"A name that matches no variable is an error.",
			Params:  []Param{{Name: "name", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name: "systemVar",
			Doc: "Returns the plaintext value of the global configuration variable named name, with no partition lookup. " +
				"A name that matches no variable is an error.",
			Params:  []Param{{Name: "name", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name: "secret",
			Doc: "Returns the decrypted value of the secret named name, looked up among the partition secrets and then the global ones. " +
				"A name that matches no secret is an error, and the value must never be logged.",
			Params:  []Param{{Name: "name", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},
		{
			Name: "systemSecret",
			Doc: "Returns the decrypted value of the global secret named name, with no partition lookup. " +
				"A name that matches no secret is an error, and the value must never be logged.",
			Params:  []Param{{Name: "name", Type: TypeString}},
			Returns: TypeString,
			Tier:    TierM,
		},

		// ---- Functions: relationship traversals (P) ----
		traversal("parentOf", "the parents of the rows selected by match, following their parent edges"),
		traversal("childOf", "the children of the rows selected by match, the rows whose parent edge points at one of them"),
		traversal("aliasOf", "the rows sharing an alias group with the rows selected by match"),
		traversal("equals", "the rows joined to the rows selected by match by an equals edge"),
		traversal("references", "the rows joined to the rows selected by match by a references edge"),
		traversal("contains", "the members of the collection rows selected by match, following their contains edges (the graph traversal: the substring test is string.includes)"),
		traversal("owns", "the rows joined to the rows selected by match by an owns edge, in either direction"),
		traversal("createdBy", "the creators of the rows selected by match, following their createdBy edges"),
		{
			// ids follows no edge, so a label on it would always be a mistake
			// (the engine refuses one today for the same reason:
			// component/memql/executor_relationship.go).
			Name:    "ids",
			Doc:     "Returns the rows selected by match as id-only rows, without payload or schema. It follows no edge, so it takes no label.",
			Params:  []Param{{Name: "match", Type: TypeLambda}},
			Returns: TypeRows,
			Tier:    TierP,
		},

		// ---- Methods on a string ----
		{
			Name:     "includes",
			Receiver: TypeString,
			Doc:      "Reports whether sub occurs in the string. An absent string or an absent sub answers false.",
			Params:   []Param{{Name: "sub", Type: TypeString}},
			Returns:  TypeBool,
			Tier:     TierP,
			Retired:  []string{"contains(s, sub)"},
		},
		{
			Name:     "count",
			Receiver: TypeString,
			Doc:      "Returns the number of characters in the string, counting Unicode code points rather than bytes. An absent string counts as zero.",
			Returns:  TypeNumber,
			Tier:     TierM,
		},

		// ---- Methods on a list ----
		//
		// count, any and all are P: over a row array field they lower to SQL
		// (jsonb_array_length, EXISTS over jsonb_array_elements). Over any
		// other list -- an arg, a step result -- they run in process like the
		// M methods; the tier names what the method CAN push down.
		{
			Name:     "count",
			Receiver: TypeList,
			Doc:      "Returns the number of elements in the list. An absent list counts as zero.",
			Returns:  TypeNumber,
			Tier:     TierP,
			Retired:  []string{"len(x)", "count(x)"},
		},
		{
			Name:     "any",
			Receiver: TypeList,
			Doc:      "Reports whether pred holds for at least one element of the list. An empty or absent list answers false.",
			Params:   []Param{{Name: "pred", Type: TypeLambda}},
			Returns:  TypeBool,
			Tier:     TierP,
		},
		{
			Name:     "all",
			Receiver: TypeList,
			Doc:      "Reports whether pred holds for every element of the list. An empty or absent list answers true.",
			Params:   []Param{{Name: "pred", Type: TypeLambda}},
			Returns:  TypeBool,
			Tier:     TierP,
		},
		{
			Name:     "where",
			Receiver: TypeList,
			Doc:      "Returns the elements of the list that pred holds for, in order. An absent list yields an empty list.",
			Params:   []Param{{Name: "pred", Type: TypeLambda}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "select",
			Receiver: TypeList,
			Doc:      "Returns fn applied to each element of the list, in order. An absent list yields an empty list.",
			Params:   []Param{{Name: "fn", Type: TypeLambda}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "first",
			Receiver: TypeList,
			Doc:      "Returns the first element of the list. An empty or absent list yields an absent value; to take the first match, filter first: `xs.where(x => p).first()`.",
			Returns:  TypeAny,
			Tier:     TierM,
			Retired:  []string{"first(x)"},
		},
		{
			Name:     "last",
			Receiver: TypeList,
			Doc:      "Returns the last element of the list. An empty or absent list yields an absent value.",
			Returns:  TypeAny,
			Tier:     TierM,
			Retired:  []string{"last(x)"},
		},
		{
			Name:     "single",
			Receiver: TypeList,
			Doc:      "Returns the one element of the list. Any other number of elements, none included, is an error; to take the one match, filter first: `xs.where(x => p).single()`.",
			Returns:  TypeAny,
			Tier:     TierM,
		},
		{
			Name:     "empty",
			Receiver: TypeList,
			Doc:      "Reports whether the list has no elements. An absent list is empty.",
			Returns:  TypeBool,
			Tier:     TierM,
		},
		{
			Name:     "sum",
			Receiver: TypeList,
			Doc:      "Returns the sum of fn over the elements of the list. An empty or absent list sums to zero, and fn returning a non-number is an error.",
			Params:   []Param{{Name: "fn", Type: TypeLambda}},
			Returns:  TypeNumber,
			Tier:     TierM,
		},
		{
			Name:     "min",
			Receiver: TypeList,
			Doc:      "Returns the smallest value of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error.",
			Params:   []Param{{Name: "fn", Type: TypeLambda}},
			Returns:  TypeNumber,
			Tier:     TierM,
		},
		{
			Name:     "max",
			Receiver: TypeList,
			Doc:      "Returns the largest value of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error.",
			Params:   []Param{{Name: "fn", Type: TypeLambda}},
			Returns:  TypeNumber,
			Tier:     TierM,
		},
		{
			Name:     "avg",
			Receiver: TypeList,
			Doc:      "Returns the mean of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error.",
			Params:   []Param{{Name: "fn", Type: TypeLambda}},
			Returns:  TypeNumber,
			Tier:     TierM,
		},
		{
			Name:     "orderBy",
			Receiver: TypeList,
			Doc:      "Returns the list sorted ascending by key, elements with equal keys keeping their order. An absent list yields an empty list.",
			Params:   []Param{{Name: "key", Type: TypeLambda}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "orderByDesc",
			Receiver: TypeList,
			Doc:      "Returns the list sorted descending by key, elements with equal keys keeping their order. An absent list yields an empty list.",
			Params:   []Param{{Name: "key", Type: TypeLambda}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "groupBy",
			Receiver: TypeList,
			Doc:      "Returns one group per distinct value of key, in first-seen order, each a map holding key and items. An absent list yields an empty list.",
			Params:   []Param{{Name: "key", Type: TypeLambda}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "distinct",
			Receiver: TypeList,
			Doc:      "Returns the list with repeated elements dropped, keeping the first of each; with key, two elements repeat when key gives both the same value. An absent list yields an empty list.",
			Params:   []Param{{Name: "key", Type: TypeLambda, Optional: true}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "take",
			Receiver: TypeList,
			Doc:      "Returns the first n elements of the list, or all of them when there are fewer; a negative n takes none. An absent list yields an empty list.",
			Params:   []Param{{Name: "n", Type: TypeNumber}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "skip",
			Receiver: TypeList,
			Doc:      "Returns the list without its first n elements; a negative n skips none. An absent list yields an empty list.",
			Params:   []Param{{Name: "n", Type: TypeNumber}},
			Returns:  TypeList,
			Tier:     TierM,
		},
		{
			Name:     "reduce",
			Receiver: TypeList,
			Doc:      "Returns seed folded through the list, calling fn with the accumulator and each element in turn. An empty or absent list returns seed unchanged.",
			Params:   []Param{{Name: "seed", Type: TypeAny}, {Name: "fn", Type: TypeLambda}},
			Returns:  TypeAny,
			Tier:     TierM,
		},
		{
			Name:     "nodes",
			Receiver: TypeList,
			Doc:      "Returns the rows of a query result as a list; a list comes back unchanged. An absent value yields an empty list.",
			Returns:  TypeList,
			Tier:     TierM,
		},
	}
}
