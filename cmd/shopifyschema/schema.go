// Package main -- schema.go: the Admin GraphQL introspection document, how it
// is obtained, and the index the emitters read it through.
//
// The document comes from the token-free schema proxy Shopify's own codegen
// preset downloads from (https://shopify.dev/admin-graphql-direct-proxy/{version}).
// No store, no app and no access token is involved -- the schema is public and
// the same for every merchant on a given version, which is what makes the
// generated mirror a property of the VERSION rather than of one store.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ProxyURL is the introspection endpoint for a version. Exported as a
// function rather than a constant so the version stays a parameter
// everywhere -- the pin is the whole point of D1.
func ProxyURL(version string) string {
	return "https://shopify.dev/admin-graphql-direct-proxy/" + version
}

// introspectionQuery asks for exactly what the emitters read: the type
// list with fields, their argument names, enum values, interfaces and
// possible types. Descriptions are requested and then DROPPED by prune --
// they are two thirds of the six-megabyte response and nothing generates
// from them.
const introspectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    types { ...FullType }
  }
}
fragment FullType on __Type {
  kind
  name
  fields(includeDeprecated: true) {
    name
    args { name defaultValue type { ...TypeRef } }
    type { ...TypeRef }
    isDeprecated
    deprecationReason
  }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name isDeprecated }
  possibleTypes { ...TypeRef }
}
fragment TypeRef on __Type {
  kind
  name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }
}`

// TypeRef is one node of GraphQL's wrapper chain (NON_NULL / LIST around a
// named type). Named types have Name set and OfType nil; wrappers are the
// other way round.
type TypeRef struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	OfType *TypeRef `json:"ofType,omitempty"`
}

// Named walks the wrapper chain to the named type at the bottom.
func (t *TypeRef) Named() string {
	for cur := t; cur != nil; cur = cur.OfType {
		if cur.Name != "" {
			return cur.Name
		}
	}
	return ""
}

// NonNull reports whether the OUTERMOST wrapper is NON_NULL, which is what
// decides whether the mirrored field is required.
func (t *TypeRef) NonNull() bool { return t != nil && t.Kind == "NON_NULL" }

// IsList reports whether a LIST wrapper appears anywhere in the chain.
func (t *TypeRef) IsList() bool {
	for cur := t; cur != nil; cur = cur.OfType {
		if cur.Kind == "LIST" {
			return true
		}
	}
	return false
}

// InputValue is a field argument. Only the name is used (to decide whether a
// connection accepts `query:`, and therefore whether the domain can be
// reconciled by `updated_at`), but the type rides along so the fixture stays
// a faithful subset.
type InputValue struct {
	Name         string   `json:"name"`
	DefaultValue string   `json:"defaultValue,omitempty"`
	Type         *TypeRef `json:"type,omitempty"`
}

// Field is one field of an object or interface type.
type Field struct {
	Name              string       `json:"name"`
	Args              []InputValue `json:"args,omitempty"`
	Type              *TypeRef     `json:"type"`
	IsDeprecated      bool         `json:"isDeprecated,omitempty"`
	DeprecationReason string       `json:"deprecationReason,omitempty"`
}

// HasArg reports whether the field accepts an argument by that name.
func (f *Field) HasArg(name string) bool {
	for _, a := range f.Args {
		if a.Name == name {
			return true
		}
	}
	return false
}

// EnumValue is one member of an enum type.
type EnumValue struct {
	Name         string `json:"name"`
	IsDeprecated bool   `json:"isDeprecated,omitempty"`
}

// Type is one entry of the schema's type list.
type Type struct {
	Kind          string      `json:"kind"`
	Name          string      `json:"name"`
	Fields        []Field     `json:"fields,omitempty"`
	Interfaces    []TypeRef   `json:"interfaces,omitempty"`
	EnumValues    []EnumValue `json:"enumValues,omitempty"`
	PossibleTypes []TypeRef   `json:"possibleTypes,omitempty"`
}

// Field returns the named field, or nil.
func (t *Type) Field(name string) *Field {
	if t == nil {
		return nil
	}
	for i := range t.Fields {
		if t.Fields[i].Name == name {
			return &t.Fields[i]
		}
	}
	return nil
}

// Schema is the introspection document, trimmed to what the generator reads.
type Schema struct {
	Version   string  `json:"version"`
	QueryType string  `json:"queryType"`
	Types     []*Type `json:"types"`

	index map[string]*Type
}

// Index builds (once) the name -> type map the emitters walk.
func (s *Schema) Index() map[string]*Type {
	if s.index == nil {
		s.index = make(map[string]*Type, len(s.Types))
		for _, t := range s.Types {
			s.index[t.Name] = t
		}
	}
	return s.index
}

// Lookup resolves a type by name.
func (s *Schema) Lookup(name string) *Type { return s.Index()[name] }

// MustLookup resolves a type by name or reports which one is missing --
// a missing type means the allowlist names something this version dropped,
// which is exactly the quarterly-bump signal the drift gate exists for.
func (s *Schema) MustLookup(name string) (*Type, error) {
	t := s.Lookup(name)
	if t == nil {
		return nil, fmt.Errorf("type %q is not in the %s schema (a version bump may have renamed or removed it)", name, s.Version)
	}
	return t, nil
}

// rawIntrospection is the shape the proxy answers with.
type rawIntrospection struct {
	Data struct {
		Schema struct {
			QueryType    struct{ Name string } `json:"queryType"`
			MutationType struct{ Name string } `json:"mutationType"`
			Types        []*Type               `json:"types"`
		} `json:"__schema"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// FetchSchema introspects the live proxy for a version.
func FetchSchema(version string, timeout time.Duration) (*Schema, error) {
	payload, err := json.Marshal(map[string]string{"query": introspectionQuery})
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPost, ProxyURL(version), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("introspect %s: HTTP %d (a version the proxy does not serve answers 404)", version, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return decodeIntrospection(version, body)
}

// ReadSchemaFile loads a recorded fixture. Accepts both the raw proxy
// response and the pruned fixture this tool records, so a freshly captured
// response can be fed straight in.
func ReadSchemaFile(path string) (*Schema, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pruned Schema
	if err := json.Unmarshal(body, &pruned); err == nil && len(pruned.Types) > 0 && pruned.QueryType != "" {
		return &pruned, nil
	}
	return decodeIntrospection("", body)
}

func decodeIntrospection(version string, body []byte) (*Schema, error) {
	var raw rawIntrospection
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode introspection: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("introspection returned errors: %s", raw.Errors[0].Message)
	}
	if len(raw.Data.Schema.Types) == 0 {
		return nil, fmt.Errorf("introspection returned no types")
	}
	return &Schema{
		Version:   version,
		QueryType: raw.Data.Schema.QueryType.Name,
		Types:     raw.Data.Schema.Types,
	}, nil
}

// Prune returns a copy of the schema holding only the types reachable from
// the given roots, plus the query root and the webhook-topic enum.
//
// Reachability is what makes the fixture checkable in: the full 2026-07
// document is about six megabytes and 3,552 types, most of them mutation
// inputs and payloads with no mirror value. A pruned fixture regenerates the
// tree byte-identically because generation only ever reads reachable types --
// and if that ever stops being true, the drift gate is what says so.
func (s *Schema) Prune(roots []string, keep ...string) *Schema {
	idx := s.Index()
	// The SHALLOWEST depth each type was reached at, not merely whether it
	// was reached.
	//
	// A plain seen-set is wrong here and wrong SILENTLY: a type first
	// visited near the bound -- Order arrives at depth 6 through
	// Customer.lastOrder -- is marked seen with its own fields unvisited,
	// and the later visit from the root at depth 0 returns immediately. The
	// fixture then omits types the generator needs, and the field that
	// referenced them is dropped from the mirror with no error anywhere.
	// Measured: PurchasingEntity, the union carrying an order's B2B company.
	seen := map[string]int{}
	var visit func(name string, depth int)
	visit = func(name string, depth int) {
		if name == "" || depth > pruneDepth {
			return
		}
		if best, ok := seen[name]; ok && best <= depth {
			return
		}
		t := idx[name]
		if t == nil {
			return
		}
		seen[name] = depth
		for _, f := range t.Fields {
			visit(f.Type.Named(), depth+1)
			for _, a := range f.Args {
				visit(a.Type.Named(), depth+1)
			}
		}
		for _, i := range t.Interfaces {
			visit(i.Named(), depth+1)
		}
		for _, p := range t.PossibleTypes {
			visit(p.Named(), depth+1)
		}
	}
	for _, r := range roots {
		visit(r, 0)
	}
	for _, k := range keep {
		visit(k, pruneDepth) // named explicitly: kept, but not a new frontier
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &Schema{Version: s.Version, QueryType: s.QueryType}
	for _, n := range names {
		out.Types = append(out.Types, idx[n])
	}
	return out
}

// pruneDepth bounds the reachability walk. Six levels reaches every type a
// selection set of depth three can name, with room to spare; without a bound
// the walk pulls in the whole schema through Node/HasMetafields.
const pruneDepth = 6

// WriteSchemaFile records a fixture as canonical, indented JSON so a diff
// between two versions is readable.
func WriteSchemaFile(path string, s *Schema) error {
	body, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(string(body), "\n") {
		body = append(body, '\n')
	}
	return os.WriteFile(path, body, 0o644)
}
