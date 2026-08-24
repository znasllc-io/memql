// Package main -- allowlist.go: the reviewed list of what gets mirrored.
//
// The allowlist is the SUBSTANCE of decision D1 and the generator is the
// fidelity. Shopify's Admin schema carries 3,552 types in 2026-07, most of
// them mutation inputs, payloads and app-scoped objects with nothing to
// mirror. Generating everything is noise; hand-writing a subset is the
// "limited functionality" the requirement refused, and it lags every quarter.
// So the list of ROOT TYPES is reviewed by a person and everything below a
// root -- every field, every enum value, every child connection's shape --
// comes from the schema.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reconciliation modes. Which one a domain gets is a property of the Admin
// API, not a preference: a root connection that accepts `query:` can be
// paged by `updated_at:>`, and one that does not can only be re-listed whole.
const (
	// ReconcileUpdatedAt pages the root connection with `updated_at:>since`.
	ReconcileUpdatedAt = "updated_at"
	// ReconcileFullRelist re-lists the whole domain on a cadence and
	// tombstones rows the origin no longer returns. The polling-only set of
	// spec D3 -- the domains Shopify publishes no webhook topic for.
	ReconcileFullRelist = "full_relist"
	// ReconcileNone is for a domain with neither: a singleton (Shop) or a
	// child materialised only with its parent.
	ReconcileNone = "none"
)

// Topic actions. What the connector does when a topic arrives.
const (
	// ActionUpsert fetches the object by GID with the generated selection
	// set and writes it. Every create / update topic.
	ActionUpsert = "upsert"
	// ActionDelete writes a tombstone without a fetch. Every delete topic.
	ActionDelete = "delete"
)

// Child is a connection on a parent type that carries DATA rather than
// references, and is therefore materialised as its own concept.
type Child struct {
	// Connection is the field name on the parent (e.g. "lineItems").
	Connection string `yaml:"connection"`
	// Type is the GraphQL type of the connection's node.
	Type string `yaml:"type"`
	// Page is how many to pull in a single fetch's nested selection.
	// Bulk operations page for themselves, so this only bounds the
	// webhook-triggered fetch.
	Page int `yaml:"page,omitempty"`
}

// Entry is one allowlisted root type.
type Entry struct {
	// Type is the GraphQL object (or interface) type name.
	Type string `yaml:"type"`
	// Concept is the MemQL concept name; the canonical id is
	// v1:shopify:<concept>.
	Concept string `yaml:"concept"`
	// Query is the QueryRoot connection field the domain is listed
	// through. Empty for a singleton or a child-only type.
	Query string `yaml:"query,omitempty"`
	// Singleton names a QueryRoot field returning ONE object (shop).
	Singleton string `yaml:"singleton,omitempty"`
	// Scopes are the Admin access scopes the domain needs. The runbook's
	// scope table and the portal's scope check both read this.
	Scopes []string `yaml:"scopes,omitempty"`
	// Reconcile is one of the Reconcile* constants.
	Reconcile string `yaml:"reconcile"`
	// Cadence is how often a full re-list runs (Go duration). Only
	// meaningful with ReconcileFullRelist.
	Cadence string `yaml:"cadence,omitempty"`
	// Bulk says the domain is backfilled through a Bulk Operation.
	Bulk bool `yaml:"bulk"`
	// Topics maps a WebhookSubscriptionTopic enum value to its action.
	Topics map[string]string `yaml:"topics,omitempty"`
	// Children are the data-carrying connections materialised as their own
	// concepts. Each named type must itself be an allowlist entry.
	Children []Child `yaml:"children,omitempty"`
	// References are connections kept as a []string of GIDs rather than
	// materialised. Anything not listed here or in Children is omitted from
	// the mirror -- a connection is expensive and silence is the default.
	References []string `yaml:"references,omitempty"`
	// Skip names fields to omit even though they map cleanly: the ones
	// whose cost or churn is not worth mirroring.
	Skip []string `yaml:"skip,omitempty"`

	// parent is derived, not authored: the type that lists this one as a
	// child. Empty for a root-only type.
	parent string
	// parentConnection is the connection on the parent that carries it.
	parentConnection string
}

// Allowlist is the whole file.
type Allowlist struct {
	// APIVersion pins the Admin GraphQL version the tree was generated
	// from. The quarterly bump changes this line and nothing else by hand.
	APIVersion string  `yaml:"apiVersion"`
	Types      []Entry `yaml:"types"`

	byType map[string]*Entry
}

// LoadAllowlist reads and validates the allowlist.
func LoadAllowlist(path string) (*Allowlist, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var al Allowlist
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&al); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := al.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &al, nil
}

func (a *Allowlist) validate() error {
	if strings.TrimSpace(a.APIVersion) == "" {
		return fmt.Errorf("apiVersion is required -- the tree is a property of the pinned version")
	}
	a.byType = make(map[string]*Entry, len(a.Types))
	concepts := map[string]string{}
	for i := range a.Types {
		e := &a.Types[i]
		if e.Type == "" || e.Concept == "" {
			return fmt.Errorf("entry %d: both type and concept are required", i)
		}
		if prev, dup := a.byType[e.Type]; dup {
			return fmt.Errorf("type %q listed twice (second as concept %q, first as %q)", e.Type, e.Concept, prev.Concept)
		}
		if prev, dup := concepts[e.Concept]; dup {
			return fmt.Errorf("concept %q claimed by both %s and %s", e.Concept, prev, e.Type)
		}
		switch e.Reconcile {
		case ReconcileUpdatedAt, ReconcileFullRelist, ReconcileNone:
		case "":
			return fmt.Errorf("%s: reconcile is required (%s | %s | %s)", e.Type, ReconcileUpdatedAt, ReconcileFullRelist, ReconcileNone)
		default:
			return fmt.Errorf("%s: reconcile %q is not a mode", e.Type, e.Reconcile)
		}
		if e.Reconcile == ReconcileFullRelist && e.Cadence == "" {
			return fmt.Errorf("%s: a full re-list needs a cadence -- it is the only thing that bounds the drift window", e.Type)
		}
		for topic, action := range e.Topics {
			if action != ActionUpsert && action != ActionDelete {
				return fmt.Errorf("%s: topic %s has action %q (want %q or %q)", e.Type, topic, action, ActionUpsert, ActionDelete)
			}
		}
		concepts[e.Concept] = e.Type
		a.byType[e.Type] = e
	}
	// Second pass: children must be allowlisted, and a type may have only
	// one parent -- parentGid is a single field, so two parents would make
	// the row's lineage ambiguous and the __parentId stream unattributable.
	for i := range a.Types {
		parent := &a.Types[i]
		for _, ch := range parent.Children {
			child, ok := a.byType[ch.Type]
			if !ok {
				return fmt.Errorf("%s.%s names child type %q, which is not an allowlist entry", parent.Type, ch.Connection, ch.Type)
			}
			if child.parent != "" && child.parent != parent.Type {
				return fmt.Errorf("type %q is claimed as a child by both %s and %s -- parentGid holds one lineage", ch.Type, child.parent, parent.Type)
			}
			child.parent = parent.Type
			child.parentConnection = ch.Connection
		}
	}
	// Topic uniqueness across the whole list: two entries claiming one topic
	// means the router has to pick, and whichever it picks the other domain
	// silently stops updating.
	owner := map[string]string{}
	for i := range a.Types {
		e := &a.Types[i]
		for topic := range e.Topics {
			if prev, dup := owner[topic]; dup {
				return fmt.Errorf("topic %s is claimed by both %s and %s", topic, prev, e.Type)
			}
			owner[topic] = e.Type
		}
	}
	return nil
}

// Entry resolves an allowlist entry by GraphQL type name.
func (a *Allowlist) Entry(typeName string) *Entry { return a.byType[typeName] }

// Sorted returns the entries in a stable order: concept name, so the
// generated tree's file order never depends on how the YAML was edited.
func (a *Allowlist) Sorted() []*Entry {
	out := make([]*Entry, 0, len(a.Types))
	for i := range a.Types {
		out = append(out, &a.Types[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Concept < out[j].Concept })
	return out
}

// RootTypeNames lists every allowlisted GraphQL type, for the fixture's
// reachability walk.
func (a *Allowlist) RootTypeNames() []string {
	out := make([]string, 0, len(a.Types)+1)
	for i := range a.Types {
		out = append(out, a.Types[i].Type)
	}
	sort.Strings(out)
	return out
}

// Parent reports the type that materialises this one as a child, and the
// connection it arrives on.
func (e *Entry) Parent() (string, string) { return e.parent, e.parentConnection }

// AllScopes is the union of every entry's scopes -- the app's scope list,
// which the runbook prints and the portal checks a store's grant against.
func (a *Allowlist) AllScopes() []string {
	seen := map[string]bool{}
	for i := range a.Types {
		for _, s := range a.Types[i].Scopes {
			seen[s] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
